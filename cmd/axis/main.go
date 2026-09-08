// axis: 常驻 MCP 服务, 提供多语言 LSP 语义工具 + codegraph 粗探索 + memory 知识库。
// 默认 stdio; -http 启动 Streamable HTTP (绑定回环), 供 opencode 等 agent 常驻调用。
// 架构: 一个服务 + 多个子插件 (LSP/codegraph/memory), 由 axis 壳组装统一注册。
// 生命周期: LSP 懒加载/空闲回收/LRU 上限; 会话注册/心跳/注销; codegraph CLI 即用即走。
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/Porter-Key/axis/internal/axis"
	"github.com/Porter-Key/axis/internal/config"
	"github.com/Porter-Key/axis/internal/logger"
)

func main() {
	httpMode := flag.Bool("http", false, "以 HTTP (Streamable HTTP) 模式启动")
	addr := flag.String("addr", "127.0.0.1:1940", "HTTP 监听地址 (仅 -http)")
	cfgPath := flag.String("config", "", "配置文件路径 (默认按约定搜索)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(2)
	}

	// 结构化日志 (JSONL) — 先于一切初始化
	if cfg.Log.Path == "" {
		cfg.Log.Path = logger.DefaultPath()
	}
	if err := logger.Init(logger.Config{Path: cfg.Log.Path, Level: cfg.Log.Level, Also: cfg.Log.Also}); err != nil {
		fmt.Fprintf(os.Stderr, "logger: %v\n", err)
		os.Exit(2)
	}
	logger.L().Info("server starting", "http", *httpMode, "addr", *addr, "config", cfgPath, "log", cfg.Log.Path)

	app, err := axis.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "axis: %v\n", err)
		os.Exit(2)
	}
	if err := app.Start(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "start plugins: %v\n", err)
		os.Exit(2)
	}
	ms := app.MCPServer()

	if !*httpMode {
		if err := server.ServeStdio(ms); err != nil {
			fmt.Fprintf(os.Stderr, "stdio serve: %v\n", err)
			os.Exit(1)
		}
		_ = app.Shutdown()
		return
	}

	// HTTP 常驻模式 (Streamable HTTP, MCP 端点默认 /mcp)
	httpServer := server.NewStreamableHTTPServer(ms)
	mux := http.NewServeMux()
	mux.Handle("/mcp", httpServer)
	app.RegisterCtrlHTTP(mux, "/ctrl/") // 控制面 (注册/心跳/状态/重载)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen %s: %v\n", *addr, err)
		os.Exit(1)
	}
	hs := &http.Server{Handler: mux}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		fmt.Fprintln(os.Stderr, "shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = app.Shutdown()
		_ = hs.Shutdown(ctx)
	}()
	fmt.Fprintf(os.Stderr, "axis HTTP listening on http://%s/mcp (ctrl: /ctrl/)\n", *addr)
	if err := hs.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "http serve: %v\n", err)
		os.Exit(1)
	}
}
