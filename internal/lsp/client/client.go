// Package lspclient 实现长连接 LSP 客户端: 语言服务器进程常驻 (registry 懒加载/回收),
// 文档按需 didOpen 后保持打开, 磁盘变更由脏标记 (内容 hash) 驱动 didChange 全量同步,
// 不再每请求 didOpen->didClose (v3 长连接模型)。
package lspclient

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Porter-Key/axis/internal/config"
	"github.com/Porter-Key/axis/internal/logger"
)

// Conn 表示一个到语言服务器的长活连接 (进程常驻, 文档状态保持, 脏标记驱动同步)。
// registry 持有它做懒加载、空闲回收与崩溃自愈。
type Conn struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	reader  *bufio.Reader
	mu      sync.Mutex // 文档状态 (openDocs/dirtyAll/docVersion/lastUse), 不横跨网络等待
	wmu     sync.Mutex // 写串行 (seq 分配 + stdin 帧), 叶子锁 (绝不反向取 mu)
	seq     int
	langID  string
	command string
	lastUse time.Time
	root    string
	caps    map[string]any

	// 并发路由: 单读循环 + pending 表, 同连接多请求在飞 (之前 mu 横跨整轮 RPC, 慢查询堵死一切)。
	pmu      sync.Mutex
	pending  map[int]chan rpcResult
	closed   bool          // readLoop 退出或 Close 后为 true (call 快速失败, 走上层自愈重启)
	loopDone chan struct{} // readLoop 退出时关闭

	// 诊断推送缓存: textDocument/publishDiagnostics 通知原文按 uri 存 (get_diagnostics 服务于此)。
	diagMu sync.Mutex
	diags  map[string]json.RawMessage

	// stop 进程生命周期取消 (与调用方 ctx 解耦: 池化长连接不能随某次请求 ctx 取消而被杀)。
	// Close 调用; Spawn 失败路径由 defer 兜底。
	stop context.CancelFunc
	// stderr 进程 stderr 环 (失败时附尾巴, 常驻成功后静默)。
	stderr *stderrCapture

	// positionEncoding 服务器使用的位置编码 (utf-16 默认; 协商后可为 utf-8/utf-32)。
	positionEncoding string

	// openDocs uri -> 已同步内容 sha256 前缀 (长连接: didOpen 后保持; 变更则 didChange)。
	openDocs map[string]string
	// docVersion uri -> 当前版本号 (didChange/didOpen 递增, LSP 要求单调)。
	docVersion map[string]int
	// dirtyAll true = 项目级失效 (fsmonitor 报告目录变更但未知具体文件), 下次任一文件请求前重读。
	dirtyAll bool
}

// JSON-RPC message
type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Spawn 启动语言服务器进程并完成 initialize 握手。长连接模型: 进程常驻, 文档保持打开, 脏标记驱动同步。
// 进程生命周期与调用方 ctx 解耦 (独立 lifeCtx): 请求取消只中断本次 spawn, 不杀池化进程;
// spawn 全程失败由 defer 兜底回收, 不留僵尸。
func Spawn(ctx context.Context, ad config.LangAdapter, root string, timeout time.Duration) (*Conn, error) {
	lifeCtx, stop := context.WithCancel(context.Background())
	cmd := exec.CommandContext(lifeCtx, ad.Command, ad.Args...)
	cmd.Dir = root
	stderrCap := &stderrCapture{}
	ok := false
	defer func() {
		if !ok {
			stop()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
	}()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = stderrCap // 进环, 不再直透 os.Stderr 刷 journal
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s: %w", ad.Command, err)
	}
	c := &Conn{
		cmd: cmd, stdin: stdin, reader: bufio.NewReader(stdout),
		langID: ad.LanguageID, command: ad.Command, root: root,
		lastUse:  time.Now(),
		openDocs: map[string]string{},
		stop:     stop,
		stderr:   stderrCap,
		pending:  map[int]chan rpcResult{},
		loopDone: make(chan struct{}),
		diags:    map[string]json.RawMessage{},
	}
	go c.readLoop() // 必须在 initialize 之前启动: 响应靠它路由
	// initialize
	initParams := map[string]any{
		"processId": nil,
		"rootUri":   uriFromPath(root),
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"definition":         map[string]any{"linkSupport": true},
				"implementation":     map[string]any{"linkSupport": true},
				"typeDefinition":     map[string]any{"linkSupport": true},
				"hover":              map[string]any{"contentFormat": []string{"markdown", "plaintext"}},
				"references":         map[string]any{},
				"declaration":        map[string]any{"linkSupport": true},
				"rename":             map[string]any{"prepareSupport": true},
				"documentSymbol":     map[string]any{},
				"codeAction":         map[string]any{"dynamicRegistration": true},
				"diagnostic":         map[string]any{"dynamicRegistration": true},
				"formatting":         map[string]any{},
				"rangeFormatting":    map[string]any{},
				"signatureHelp":      map[string]any{"signatureInformation": map[string]any{"parameterInformation": map[string]any{"labelOffsetSupport": true}}},
				"publishDiagnostics": map[string]any{},
			},
			"workspace": map[string]any{
				"symbol": map[string]any{},
			},
			// 声明支持 utf-8/utf-16 位置编码 (LSP 3.17)。gopls/rust-analyzer 会回 utf-8,
			// 其余默认 utf-16。后续所有 line/character 按服务器编码换算。
			"general": map[string]any{
				"positionEncodings": []string{"utf-8", "utf-16"},
			},
		},
	}
	if ad.InitOptions != nil {
		initParams["initializationOptions"] = ad.InitOptions
	}
	var res rpcMsg
	if err := c.call(ctx, "initialize", initParams, &res, timeout); err != nil {
		// defer 兜底 kill; 这里只附 stderr 尾巴 (之前排障抓瞎的根因)
		return nil, fmt.Errorf("initialize %s: %w\n[%s stderr tail]\n%s", ad.Command, err, ad.Command, stderrCap.tail(2000))
	}
	if res.Error != nil {
		return nil, fmt.Errorf("initialize %s: %s\n[%s stderr tail]\n%s", ad.Command, res.Error.Message, ad.Command, stderrCap.tail(2000))
	}
	// 解析 initialize result: 规范为 {capabilities:{...}, serverInfo:{...}},
	// 实际 result 顶层含 capabilities 键。解出 capabilities 存 c.caps。
	var initResult struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	_ = json.Unmarshal(res.Result, &initResult)
	if initResult.Capabilities != nil {
		c.caps = initResult.Capabilities
	} else {
		_ = json.Unmarshal(res.Result, &c.caps) // 兼容直接平铺的 server
	}
	// 协商 positionEncoding: 服务器支持 utf-8 且未明确只用 utf-16 → 用 utf-8
	c.positionEncoding = "utf-16" // LSP 默认
	if c.supportsPositionEncoding("utf-8") {
		c.positionEncoding = "utf-8"
	}
	// initialized 通知
	if err := c.notify("initialized", map[string]any{}); err != nil {
		return nil, fmt.Errorf("initialized %s: %w\n[%s stderr tail]\n%s", ad.Command, err, ad.Command, stderrCap.tail(2000))
	}
	// 调用方在 spawn 过程中取消 → 不泄漏进程 (defer 兜底 kill, 这里转 ctx 错误)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	ok = true
	return c, nil
}

// CallWithDoc 在常驻连接上执行一次文件请求 (长连接语义):
//   - 首次请求该文件 → didOpen (读最新盘, 记录内容 hash, 保持打开)
//   - 后续请求 → 若内容 hash 变更 (或 dirtyAll) → didChange 全量同步 → 请求
//   - 不 didClose (长连接)
//
// 并发: 文档同步段持 c.mu (短); 实际请求经 c.call 多路并发, 不再被慢查询堵死。
func (c *Conn) CallWithDoc(ctx context.Context, method string, filePath string, extra map[string]any, timeout time.Duration) (json.RawMessage, error) {
	c.mu.Lock()
	c.lastUse = time.Now()

	uri := uriFromPath(filePath)
	text, err := os.ReadFile(filePath)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	hash := fileHash(text)

	prevHash, opened := c.openDocs[uri]
	needSync := !opened || c.dirtyAll || prevHash != hash
	if c.dirtyAll {
		c.dirtyAll = false
	}

	if !opened {
		// 首次: didOpen (全量文本)
		didOpen := map[string]any{
			"textDocument": map[string]any{
				"uri":        uri,
				"languageId": c.langID,
				"version":    c.nextVersion(uri),
				"text":       string(text),
			},
		}
		if err := c.writeMsg(method_notify, "textDocument/didOpen", didOpen); err != nil {
			c.mu.Unlock()
			return nil, err
		}
		c.openDocs[uri] = hash
	} else if needSync {
		// 已开但磁盘变更: didChange (full text, range 为空 = 全量)
		didChange := map[string]any{
			"textDocument": map[string]any{
				"uri":     uri,
				"version": c.nextVersion(uri),
			},
			"contentChanges": []map[string]any{
				{"text": string(text)},
			},
		}
		if err := c.writeMsg(method_notify, "textDocument/didChange", didChange); err != nil {
			c.mu.Unlock()
			return nil, err
		}
		c.openDocs[uri] = hash
	}
	c.mu.Unlock() // 文档状态已定; 下方网络往返不再持锁 (多路并发)

	params := map[string]any{"textDocument": map[string]any{"uri": uri}}
	for k, v := range extra {
		params[k] = v
	}
	var res rpcMsg
	if err := c.call(ctx, method, params, &res, timeout); err != nil {
		return nil, err
	}
	if res.Error != nil {
		return nil, fmt.Errorf("%s: %s", method, res.Error.Message)
	}
	return res.Result, nil
}

// nextVersion 返回该文档下一个版本号 (长连接内递增)。
func (c *Conn) nextVersion(uri string) int {
	// openDocs 值存 hash; 版本号用单独递增计数简化: 每次变更 +1。
	// 这里用 hash 变更次数近似: 维护 docVersions map 更准, 但 LSP 仅要求递增, 用自增全局亦可。
	if c.docVersion == nil {
		c.docVersion = map[string]int{}
	}
	c.docVersion[uri]++
	return c.docVersion[uri]
}

// SyncFile 强制同步文件 (didChange 全量), 供重索引/显式刷新。若文件未打开则 didOpen。
func (c *Conn) SyncFile(ctx context.Context, filePath string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastUse = time.Now()

	uri := uriFromPath(filePath)
	text, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	hash := fileHash(text)
	if _, opened := c.openDocs[uri]; !opened {
		didOpen := map[string]any{
			"textDocument": map[string]any{
				"uri":        uri,
				"languageId": c.langID,
				"version":    c.nextVersion(uri),
				"text":       string(text),
			},
		}
		if err := c.writeMsg(method_notify, "textDocument/didOpen", didOpen); err != nil {
			return err
		}
	} else {
		didChange := map[string]any{
			"textDocument": map[string]any{"uri": uri, "version": c.nextVersion(uri)},
			"contentChanges": []map[string]any{
				{"text": string(text)},
			},
		}
		if err := c.writeMsg(method_notify, "textDocument/didChange", didChange); err != nil {
			return err
		}
	}
	c.openDocs[uri] = hash
	c.dirtyAll = false
	return nil
}

// NeedsSync 该文件是否需同步 (磁盘 hash 与已同步不一致, 或项目级失效未清)。
func (c *Conn) NeedsSync(filePath string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dirtyAll {
		return true
	}
	uri := uriFromPath(filePath)
	prev, opened := c.openDocs[uri]
	if !opened {
		return true
	}
	text, err := os.ReadFile(filePath)
	if err != nil {
		return false // 文件不存在 → 无需同步 (调用方会报错)
	}
	return prev != fileHash(text)
}

// Invalidate 标记文件/项目失效。changedPath 为空 = 整个项目目录失效 (dirtyAll)。
func (c *Conn) Invalidate(changedPath string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if changedPath == "" {
		c.dirtyAll = true
		return
	}
	uri := uriFromPath(changedPath)
	// 仅当该文件已打开才标记 (未打开的请求时会 didOpen 读最新, 无需失效)。
	if _, ok := c.openDocs[uri]; ok {
		delete(c.openDocs, uri) // 删除 → 下次请求走 didOpen 分支重新读盘
	}
}

// fileHash 内容 sha256 前 16 位 (足够判变)。
func fileHash(b []byte) string {
	s := sha256.Sum256(b)
	return fmt.Sprintf("%x", s[:8])
}

// WorkspaceRequest 发送 workspace 级请求 (如 workspace/symbol, 无需 didOpen)。
func (c *Conn) WorkspaceRequest(ctx context.Context, method string, params map[string]any, timeout time.Duration) (json.RawMessage, error) {
	c.mu.Lock()
	c.lastUse = time.Now()
	c.mu.Unlock()
	var res rpcMsg
	if err := c.call(ctx, method, params, &res, timeout); err != nil {
		return nil, err
	}
	if res.Error != nil {
		return nil, fmt.Errorf("%s: %s", method, res.Error.Message)
	}
	return res.Result, nil
}

// LastUse 最近使用时间 (供空闲回收判断)。
func (c *Conn) LastUse() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastUse
}

// ProcessPID 返回子进程 PID (0 若未启动)。
func (c *Conn) ProcessPID() int {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// IsAlive 进程是否存活 (registry 崩溃检测用; pid 存在且未退出)。
func (c *Conn) IsAlive() bool {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return false
	}
	// 发信号 0 探测; 若进程已回收返回错误 → 判定死亡。
	// (避免 Wait 阻塞: 只探 pid, 不等待)
	return c.cmd.Process.Signal(syscall.Signal(0)) == nil
}

// Lang 返回语言 id。
func (c *Conn) Lang() string { return c.langID }

// PositionEncoding 返回协商后的位置编码 (utf-8/utf-16/utf-32)。
func (c *Conn) PositionEncoding() string { return c.positionEncoding }

// Supports 检查服务器 capability (如 textDocument/implementationProvider)。
// keyPath 用点路径: "textDocument.implementationProvider"。
// 兼容两种 capabilities 结构: gopls 嵌套在 textDocument 下, pyright 等平铺在顶层。
// 返回 capability 的 JSON 值 (bool/对象) 是否存在且非 false。
func (c *Conn) Supports(keyPath string) bool {
	if c == nil || c.caps == nil {
		return false
	}
	// 尝试完整路径
	if supportsPath(c.caps, keyPath) {
		return true
	}
	// 兼容平铺: 若 keyPath 形如 textDocument.X, 退化查顶层 X
	if i := strings.Index(keyPath, "."); i > 0 {
		return supportsPath(c.caps, keyPath[i+1:])
	}
	return false
}

func supportsPath(caps map[string]any, keyPath string) bool {
	var cur any = caps
	for _, part := range strings.Split(keyPath, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		cur, ok = m[part]
		if !ok {
			return false
		}
	}
	// capability 值: 可为 bool / 对象 / 数组 (定义 provider 时常为对象如 {idProvider:true})
	switch v := cur.(type) {
	case bool:
		return v
	case nil:
		return false
	default:
		return true
	}
}

// supportsPositionEncoding 服务器 capabilities 中是否含指定位置编码。
func (c *Conn) supportsPositionEncoding(enc string) bool {
	if c == nil || c.caps == nil {
		return false
	}
	gen, ok := c.caps["general"].(map[string]any)
	if !ok {
		return false
	}
	posEnc, ok := gen["positionEncodings"].([]any)
	if !ok {
		return false
	}
	for _, e := range posEnc {
		if s, ok := e.(string); ok && s == enc {
			return true
		}
	}
	return false
}

// Close 关闭进程 (发 shutdown/exit 后 kill, 并取消生命周期 ctx)。
// 同时失败全部在飞请求并等待读循环退出 (有界 2s, 防管道卡死)。
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stop != nil {
		c.stop() // 先停 lifeCtx (CommandContext 兜底, 进程必收)
	}
	c.writeMsgGetID(method_call, "shutdown", map[string]any{})
	c.writeMsgGetID(method_notify, "exit", nil)
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_, _ = c.cmd.Process.Wait()
	}
	c.failAllPending(fmt.Errorf("conn closed (%s %s)", c.langID, c.root))
	if c.loopDone != nil {
		select {
		case <-c.loopDone:
		case <-time.After(2 * time.Second):
		}
	}
	return nil
}

// rpcResult 一次请求的路由结果 (响应或路由级错误)。
type rpcResult struct {
	msg *rpcMsg
	err error
}

// call 发送请求并等待响应 (同连接可多路并发, 靠 readLoop 按 id 路由)。
// 超时/取消后摘除 pending (迟到响应被 readLoop 丢弃); 连接已死快速失败走上层自愈。
func (c *Conn) call(ctx context.Context, method string, params any, out *rpcMsg, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	id := c.allocID()
	ch := make(chan rpcResult, 1)
	c.pmu.Lock()
	if c.closed {
		c.pmu.Unlock()
		return fmt.Errorf("conn closed (%s %s)", c.langID, c.root)
	}
	c.pending[id] = ch
	c.pmu.Unlock()
	if err := c.writeRequest(id, method, params); err != nil {
		c.removePending(id)
		return fmt.Errorf("write %s failed: %w", method, err)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		c.removePending(id)
		return ctx.Err()
	case <-timer.C:
		c.removePending(id)
		return fmt.Errorf("request %s timeout after %v", method, timeout)
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		*out = *r.msg
		return nil
	}
}

// allocID 分配唯一请求 id (wmu 保护)。
func (c *Conn) allocID() int {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.seq++
	return c.seq
}

// writeRequest 写一条带 id 的请求帧 (字节级原子由 writeFrame 的 wmu 保证)。
func (c *Conn) writeRequest(id int, method string, params any) error {
	raw, _ := json.Marshal(params)
	return c.writeFrame(rpcMsg{JSONRPC: "2.0", ID: &id, Method: method, Params: raw})
}

// removePending 摘除等待项 (超时/取消后调用; 迟到响应会被投递时丢弃)。
func (c *Conn) removePending(id int) {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	delete(c.pending, id)
}

// deliver 投递响应到等待者 (readLoop 唯一调用)。无等待项 (超时已摘除) 则丢弃。
func (c *Conn) deliver(id int, msg *rpcMsg) {
	c.pmu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.pmu.Unlock()
	if ok {
		ch <- rpcResult{msg: msg}
	}
}

// failAllPending 连接死亡时失败全部在飞请求 (readLoop 退出/Close 调用, 幂等)。
func (c *Conn) failAllPending(err error) {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	c.closed = true
	for id, ch := range c.pending {
		delete(c.pending, id)
		ch <- rpcResult{err: err}
	}
}

// readLoop 单读循环 (每连接一条 goroutine, Spawn 后启动, 读到 EOF/错即退出):
//   - 带 id 无 method = 响应 → 按 id 投递 pending (无等待项则丢弃, 如超时后迟到)
//   - 带 id 有 method = 服务端→客户端请求 (如 workspace/configuration) → 就地应答
//   - 纯通知 → publishDiagnostics 进缓存, 其余丢弃
//
// 只取 wmu (写应答帧) 与 pmu/diagMu, 永不碰 c.mu (与请求路径的 c.mu→wmu 顺序无反转)。
func (c *Conn) readLoop() {
	defer close(c.loopDone)
	for {
		msg, err := c.readMsg()
		if err != nil {
			c.failAllPending(fmt.Errorf("lsp read loop ended (%s %s): %w", c.langID, c.root, err))
			return
		}
		if msg.ID != nil && msg.Method == "" {
			c.deliver(*msg.ID, msg) // 真正的响应
			continue
		}
		if msg.ID != nil && msg.Method != "" {
			c.answerServerRequest(msg)
			continue
		}
		// 纯通知
		if msg.Method == "textDocument/publishDiagnostics" {
			c.storeDiagnostics(msg.Params)
		}
		// 其余通知 (window/logMessage 等) 丢弃
	}
}

// answerServerRequest 应答服务端→客户端请求 (id 可能与我们的请求 id 冲突,
// 如 rust-analyzer 的 workspace/diagnostic/refresh 用低 id): 必须按方法应答正确结果。
//   - client/registerCapability (动态注册, 如文件监听): 应答 {} = 成功
//   - workspace/configuration: 应答 [] (无配置项)
//   - window/workDoneProgress/create / workspace/diagnostic/refresh 等: null
func (c *Conn) answerServerRequest(msg *rpcMsg) {
	res := json.RawMessage("null")
	switch msg.Method {
	case "client/registerCapability":
		res = json.RawMessage("{}")
		logger.With("lang", c.langID, "root", c.root).Debug("server 注册动态能力", "method", msg.Method)
	case "workspace/configuration":
		res = json.RawMessage("[]")
	case "workspace/workspaceFolders":
		res = json.RawMessage("null")
	}
	resp := rpcMsg{JSONRPC: "2.0", ID: msg.ID, Result: res}
	_ = c.writeFrame(resp)
}

// storeDiagnostics 缓存一条 publishDiagnostics 推送 (按 uri 覆盖)。
func (c *Conn) storeDiagnostics(params json.RawMessage) {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.URI == "" {
		return
	}
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	c.diags[p.URI] = params
}

// DiagnosticsFor 取某文件的推送诊断缓存 (get_diagnostics 服务于此)。
// ok=false = 该连接尚无此文件的推送 (服务器没推过, 不是"零报错")。
func (c *Conn) DiagnosticsFor(filePath string) (json.RawMessage, bool) {
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	d, ok := c.diags[uriFromPath(filePath)]
	return d, ok
}

// NotifyProjectChanged 主动同步项目文件磁盘变更到 LSP 服务器。fsmonitor 检测到
// 磁盘变更后由 registry 调用。对 LSP 来说两种文件状态分开处理:
//   - 已 didOpen 的文档: LSP 规定以 didChange 内容为准, watcher 通知不影响打开文档
//     → 必须发 didChange 全量 (内容 hash 已在调用方确认变更)
//   - 未打开文档: 服务器靠磁盘读, 发 workspace/didChangeWatchedFiles 通知即可
//
// 同时清本地脏标记 (内容已同步, 无需下次请求再 didChange)。
func (c *Conn) NotifyProjectChanged(changedPaths []string) {
	if len(changedPaths) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	var watched []map[string]any
	for _, p := range changedPaths {
		uri := uriFromPath(p)
		text, err := os.ReadFile(p)
		if err != nil {
			// 文件不存在/删除: 打开文档 didClose 语义不追 (简化), watcher 通知 Deleted
			watched = append(watched, map[string]any{"uri": uri, "type": 3})
			delete(c.openDocs, uri)
			continue
		}
		hash := fileHash(text)
		if _, opened := c.openDocs[uri]; opened {
			// 已打开: didChange 全量同步 (服务器立即更新内存模型)
			didChange := map[string]any{
				"textDocument": map[string]any{"uri": uri, "version": c.nextVersion(uri)},
				"contentChanges": []map[string]any{
					{"text": string(text)},
				},
			}
			if err := c.writeMsg(method_notify, "textDocument/didChange", didChange); err != nil {
				logger.With("lang", c.langID, "root", c.root).Warn("didChange 同步失败", "path", p, "error", err.Error())
				continue
			}
			c.openDocs[uri] = hash
			logger.With("lang", c.langID, "root", c.root).Debug("已打开文档 didChange 同步", "path", p)
		} else {
			// 未打开: watcher 通知 (服务器自行读盘)
			watched = append(watched, map[string]any{"uri": uri, "type": 2})
		}
	}
	if len(watched) > 0 {
		raw, _ := json.Marshal(map[string]any{"changes": watched})
		msg := rpcMsg{JSONRPC: "2.0", Method: "workspace/didChangeWatchedFiles", Params: raw}
		if err := c.writeFrame(msg); err != nil {
			logger.With("lang", c.langID, "root", c.root).Warn("didChangeWatchedFiles 发送失败", "error", err.Error())
			return
		}
		logger.With("lang", c.langID, "root", c.root).Debug("didChangeWatchedFiles 已推送", "files", len(watched))
	}
}

// Notify 发送 notification (无响应)。供 provider 层做语言服务器特化握手
// (如 csharp-ls 的 solution/open、project/open)。
func (c *Conn) Notify(method string, params any) error {
	raw, _ := json.Marshal(params)
	msg := rpcMsg{JSONRPC: "2.0", Method: method, Params: raw}
	return c.writeFrame(msg)
}

// writeMsgGetID 发送请求/通知消息, 返回分配的 id (通知返回 0 占位, 写失败返回 0)。
// id 分配与字节写分离 (wmu 内各自原子即可; 线上 id 乱序不影响 JSON-RPC 按 id 配对)。
func (c *Conn) writeMsgGetID(kind msgKind, method string, params any) int {
	id := 0
	if kind == method_call {
		c.wmu.Lock()
		c.seq++
		id = c.seq
		c.wmu.Unlock()
	}
	msg := rpcMsg{JSONRPC: "2.0", Method: method}
	if kind == method_call {
		msg.ID = &id
	}
	raw, _ := json.Marshal(params)
	msg.Params = raw
	if err := c.writeFrame(msg); err != nil {
		return 0
	}
	return id
}

// readMsg 读一条完整的 Content-Length 帧。
func (c *Conn) readMsg() (*rpcMsg, error) {
	var contentLen int
	// 读 headers
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = trimCRLF(line)
		if line == "" {
			break // headers 结束
		}
		if strings.HasPrefix(line, "Content-Length:") {
			_, _ = fmt.Sscanf(strings.TrimPrefix(line, "Content-Length:"), "%d", &contentLen)
		}
	}
	if contentLen <= 0 {
		return nil, fmt.Errorf("invalid Content-Length: %d", contentLen)
	}
	buf := make([]byte, contentLen)
	if _, err := io.ReadFull(c.reader, buf); err != nil {
		return nil, err
	}
	var msg rpcMsg
	if err := json.Unmarshal(buf, &msg); err != nil {
		return nil, fmt.Errorf("bad json frame: %w", err)
	}
	return &msg, nil
}

func (c *Conn) notify(method string, params any) error {
	c.writeMsgGetID(method_notify, method, params)
	return nil
}

type msgKind int

const (
	method_call msgKind = iota
	method_notify
)

// writeMsg 简化包装 (供 didOpen/didChange/didClose 通知)。
func (c *Conn) writeMsg(kind msgKind, method string, params any) error {
	if kind == method_call {
		c.writeMsgGetID(kind, method, params)
		return nil
	}
	c.writeMsgGetID(kind, method, params)
	return nil
}

// writeFrame 写一条 Content-Length 帧 (wmu 保证字节级原子, 多路并发安全)。
func (c *Conn) writeFrame(msg rpcMsg) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n%s", len(data), data)
	return err
}

func trimCRLF(s string) string {
	return strings.TrimRight(s, "\r\n")
}

// uriFromPath 生成 file:// URI。
func uriFromPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "file://" + p
	}
	return "file://" + abs
}
