package positions

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// 本包: LSP 位置编码列换算的唯一成熟实现, 全仓库 (各 capacity adapter) 复用。
//
// 语义对标 gopls internal/lsp/lsppos + protocol.Mapper (BSD, 此处独立实现, 无外部依赖):
//   - 行按 \n 切分; 行尾单个 \r 不计入列 (CRLF 文件)
//   - 越界钳制到行尾 (不返回错误, 不 panic)
//   - 落在多字节字符中间 → 钳到字符尾 (宽容语义)
//   - 非法 UTF-8 字节按单字节推进 (RuneError,size=1, 不死循环)
//   - astral 字符 (>U+FFFF) = 2 个 UTF-16 单元 (surrogate pair)
//   - 空行/行尾/EOF 行为明确, 见单测表
//
// 客户端口径恒为 UTF-8 字节偏移 (axis 与各 agent 的约定); serverEnc 取值
// "utf-8"/"utf-16"/"utf-32"。未知编码一律直通 (fail-open, 不 corrupt)。
// 为避免循环 import, 本包不引用 capacity (ClientCharEncoding 常量见契约, 值 "utf-8")。

const (
	UTF8  = "utf-8"
	UTF16 = "utf-16"
	UTF32 = "utf-32"
)

// ReadLine 读文件指定行 (0-based, 不含换行符)。
// ok=false = 读不到/越界 (该位置跳过换算)。调用方提供带缓存的实现。
type ReadLine func(path string, line int) (string, bool)

// ToServer 客户端列 (UTF-8 字节) → 服务器列 (serverEnc 单位)。
func ToServer(serverEnc, line string, clientChar int) int {
	if clientChar <= 0 {
		return 0
	}
	line = stripCR(line)
	switch serverEnc {
	case UTF8:
		return min(clientChar, len(line))
	case UTF16:
		return bytesToUnits(line, clientChar)
	case UTF32:
		return bytesToRunes(line, clientChar)
	default:
		return clientChar
	}
}

// FromServer 服务器列 (serverEnc 单位) → 客户端列 (UTF-8 字节)。
func FromServer(serverEnc, line string, serverChar int) int {
	if serverChar <= 0 {
		return 0
	}
	line = stripCR(line)
	switch serverEnc {
	case UTF8:
		return min(serverChar, len(line))
	case UTF16:
		return unitsToBytes(line, serverChar)
	case UTF32:
		return runesToBytes(line, serverChar)
	default:
		return serverChar
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func stripCR(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\r' {
		return s[:len(s)-1]
	}
	return s
}

// bytesToUnits 字节偏移 → UTF-16 单元数。
func bytesToUnits(line string, b int) int {
	uo := 0
	for bo := 0; bo < len(line) && bo < b; {
		r, size := utf8.DecodeRuneInString(line[bo:])
		bo += size
		if r > 0xFFFF {
			uo += 2
		} else {
			uo++
		}
	}
	return uo
}

// unitsToBytes UTF-16 单元数 → 字节偏移。
func unitsToBytes(line string, u int) int {
	bo, uo := 0, 0
	for bo < len(line) && uo < u {
		r, size := utf8.DecodeRuneInString(line[bo:])
		bo += size
		if r > 0xFFFF {
			uo += 2
		} else {
			uo++
		}
	}
	return bo
}

// bytesToRunes 字节偏移 → rune 序号 (utf-32)。
func bytesToRunes(line string, b int) int {
	ro := 0
	for bo := 0; bo < len(line) && bo < b; {
		_, size := utf8.DecodeRuneInString(line[bo:])
		bo += size
		ro++
	}
	return ro
}

// runesToBytes rune 序号 → 字节偏移 (utf-32)。
func runesToBytes(line string, n int) int {
	bo, ro := 0, 0
	for bo < len(line) && ro < n {
		_, size := utf8.DecodeRuneInString(line[bo:])
		bo += size
		ro++
	}
	return bo
}

// ---------- 响应位置批量换算 ----------

// AdaptPositions 把 LSP 响应 JSON 中的位置换算为客户端单位 (UTF-8)。
// serverEnc == "utf-8" 时与客户端同单位, 原样返回 (零开销快路, 且不重排 key)。
// srcPath 为查询源文件 (DocumentSymbol 等无 uri 位置的归属; Location 类自带 uri 优先)。
// 覆盖: Location / LocationLink / DocumentSymbol(+children) / SymbolInformation /
// WorkspaceEdit(changes/documentChanges), 单体与数组。非位置响应、读不到行一律原样。
func AdaptPositions(raw json.RawMessage, srcPath, serverEnc string, readLine ReadLine) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return raw
	}
	if serverEnc == "" || serverEnc == UTF8 {
		return raw
	}
	if readLine == nil {
		return raw
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out := convertValue(v, srcPath, serverEnc, readLine)
	m, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return m
}

func convertValue(v any, file, enc string, readLine ReadLine) any {
	switch t := v.(type) {
	case map[string]any:
		return convertMap(t, file, enc, readLine)
	case []any:
		for i, e := range t {
			t[i] = convertValue(e, file, enc, readLine)
		}
		return t
	default:
		return v
	}
}

func convertMap(m map[string]any, file, enc string, readLine ReadLine) map[string]any {
	if u, ok := m["uri"].(string); ok && u != "" {
		if p := uriToPath(u); p != "" {
			file = p
		}
	}
	if td, ok := m["textDocument"].(map[string]any); ok {
		if u, ok := td["uri"].(string); ok && u != "" {
			if p := uriToPath(u); p != "" {
				file = p
			}
		}
	}
	// LocationLink: target* 用 targetUri 文件, originSelectionRange 用源文件
	tfile := file
	if u, ok := m["targetUri"].(string); ok && u != "" {
		if p := uriToPath(u); p != "" {
			tfile = p
		}
	}
	for k, val := range m {
		switch k {
		case "range", "targetRange", "targetSelectionRange":
			f := file
			if k != "range" {
				f = tfile
			}
			m[k] = convertRange(val, f, enc, readLine)
		case "originSelectionRange", "selectionRange":
			m[k] = convertRange(val, file, enc, readLine)
		case "changes":
			// {uri: TextEdit[]} — key 即 uri
			if cm, ok := val.(map[string]any); ok {
				for uri, edits := range cm {
					if p := uriToPath(uri); p != "" {
						cm[uri] = convertValue(edits, p, enc, readLine)
					}
				}
			}
		default:
			m[k] = convertValue(val, file, enc, readLine)
		}
	}
	return m
}

func convertRange(v any, file, enc string, readLine ReadLine) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	for _, k := range []string{"start", "end"} {
		pos, ok := m[k].(map[string]any)
		if !ok {
			continue
		}
		ln, ok1 := jsonInt(pos["line"])
		ch, ok2 := jsonInt(pos["character"])
		if !ok1 || !ok2 {
			continue
		}
		t, ok := readLine(file, ln)
		if !ok {
			continue // 读不到行 (外部依赖/虚拟文档): 原样保留
		}
		pos["character"] = FromServer(enc, t, ch)
	}
	return m
}

func jsonInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	default:
		return 0, false
	}
}

// uriToPath file:// URI 转本地路径 (非 file:// 返回 "")。
func uriToPath(uri string) string {
	if !strings.HasPrefix(uri, "file://") {
		return ""
	}
	p := strings.TrimPrefix(uri, "file://")
	p = strings.TrimPrefix(p, "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return filepath.Clean(p)
}
