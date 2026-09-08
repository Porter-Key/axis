package lsp

import (
	"encoding/json"
	"os"
	"strings"
	"sync"

	lsp_capacity "github.com/Porter-Key/axis/internal/lsp/capacity"
)

// 本文件: 位置编码宿主接线 (纯编排, 零转码逻辑)。
//
// 转码本身只发生在各 capacity provider 内 (ServerEncoding/ToServerChar/FromServerChar/
// AdaptPositionsToClient, 契约见 capacity/contract.go)。这里只做三件事:
//   - 请求前: 经 provider 把 (line, char) 换为服务器单位 (toServerPos)
//   - 响应后: 经 provider 把位置换回客户端单位 (adaptPositions)
//   - 提供文件行读取 (readSourceLine/lineReader, 纯 IO)
//
// 任一步无 provider/读不到行 → 原样放行 (fail-open = 旧行为)。

// readSourceLine 读文件指定行 (0-based, 不含换行符)。失败/越界返回 false。
func readSourceLine(path string, line int) (string, bool) {
	if line < 0 {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(data), "\n")
	if line >= len(lines) {
		return "", false
	}
	return lines[line], true
}

// lineReader 带缓存的行读取器 (一次工具调用内复用, 供 AdaptPositionsToClient 按需取行)。
// 并发安全。
type lineReader struct {
	mu    sync.Mutex
	files map[string][]string // path -> 行 (nil = 读失败, 不反复重试)
}

func newLineReader() *lineReader { return &lineReader{files: map[string][]string{}} }

func (r *lineReader) read(path string, line int) (string, bool) {
	if path == "" || line < 0 {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	lines, ok := r.files[path]
	if !ok {
		data, err := os.ReadFile(path)
		if err != nil {
			r.files[path] = nil
			return "", false
		}
		lines = strings.Split(string(data), "\n")
		r.files[path] = lines
	}
	if line >= len(lines) {
		return "", false
	}
	return lines[line], true
}

// providerFor 取语言 provider (无则 fail-open 由调用方原样放行)。
func (l *LSP) providerFor(lang string) (lsp_capacity.Provider, bool) {
	return lsp_capacity.Get(lsp_capacity.LanguageID(lang))
}

// toServerPos 经 provider 把客户端位置 (UTF-8, 见 capacity.ClientCharEncoding)
// 换为服务器单位。无 provider/读不到行 → 原样。
func (l *LSP) toServerPos(lang, path string, line, char int) (int, int) {
	prov, ok := l.providerFor(lang)
	if !ok {
		return line, char
	}
	t, ok := readSourceLine(path, line)
	if !ok {
		return line, char
	}
	return line, prov.ToServerChar(t, char)
}

// adaptPositions 经 provider 把响应位置换回客户端单位。无 provider → 原样。
// srcPath 为查询源文件 (无 uri 位置的归属)。
func (l *LSP) adaptPositions(lang, srcPath string, raw json.RawMessage) json.RawMessage {
	prov, ok := l.providerFor(lang)
	if !ok {
		return raw
	}
	return prov.AdaptPositionsToClient(raw, srcPath, newLineReader().read)
}
