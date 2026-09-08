package memory

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ---------- markdown 解析 (SSOT 写入前) ----------

// ParsedMD 解析结果: frontmatter 块与正文分离。
type ParsedMD struct {
	FrontmatterRaw string            // 原始 yaml 块文本 (无 --- 包裹), 空=无 frontmatter
	Frontmatter    map[string]string // 解析后的标量字段 (title/tags 等)
	Body           string            // 正文 (frontmatter 之后的 md 内容, 前后空白保留)
	Warnings       []string
}

// SplitFrontmatter 把整篇 md (含可选 --- frontmatter ---) 分成 frontmatter raw + body。
// frontmatter 以首行 "---" 开始、下一 "---" 结束为界; 无前导 --- 则整篇是正文。
func SplitFrontmatter(text string) ParsedMD {
	trimmed := strings.TrimPrefix(text, "\ufeff") // BOM 容错
	if !strings.HasPrefix(trimmed, "---") {
		return ParsedMD{Body: text}
	}
	// 找第二行 ---
	rest := trimmed[3:]
	// 跳过第一行末尾换行
	if strings.HasPrefix(rest, "\n") {
		rest = rest[1:]
	} else if strings.HasPrefix(rest, "\r\n") {
		rest = rest[2:]
	}
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		// 没有闭合: 视为无 frontmatter (正文)
		return ParsedMD{Body: text, Warnings: []string{"frontmatter 未闭合 (缺终止 ---), 已按正文处理"}}
	}
	fmRaw := strings.TrimSpace(rest[:idx])
	body := rest[idx+4:] // 跳过 "\n---"
	// body 去掉紧跟的换行 (保留内部格式, 只清一个前导空行)
	body = strings.TrimPrefix(body, "\n")
	body = strings.TrimPrefix(body, "\r\n")

	fm := map[string]string{}
	var warns []string
	if fmRaw != "" {
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(fmRaw), &node); err != nil {
			warns = append(warns, fmt.Sprintf("frontmatter yaml 解析失败: %v (字段忽略)", err))
		} else {
			// yaml.Unmarshal 到 Node 根是 DocumentNode, 真正的 mapping 在其 Content[0]
			mapNode := &node
			if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
				mapNode = node.Content[0]
			}
			if mapNode.Kind == yaml.MappingNode {
				for i := 0; i+1 < len(mapNode.Content); i += 2 {
					k := mapNode.Content[i].Value
					v := mapNode.Content[i+1]
					if v.Kind == yaml.ScalarNode {
						fm[k] = v.Value
					} else {
						// 复合值 (列表/映射) 用 yaml 序列化字符串保留
						b, _ := yaml.Marshal(v)
						fm[k] = strings.TrimSpace(string(b))
					}
				}
			} else {
				warns = append(warns, "frontmatter 顶层非 mapping, 字段忽略")
			}
		}
	}
	return ParsedMD{FrontmatterRaw: fmRaw, Frontmatter: fm, Body: body, Warnings: warns}
}

// ValidateBody 校验正文: 基本块结构可解析 (非空即可入库; 深度 md 校验留给 formatter,
// axis 不做格式化器职责)。
func ValidateBody(body string) error {
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("正文为空: 拒绝写入 (防止误删)")
	}
	return nil
}

// RenderMD 把文档渲染成导出 md (映射视图)。
// frontmatter 由结构化字段 (fields) 生成; 正文原样保留, 不跑任何 formatter。
func RenderMD(key string, fields map[string]string, title, body string) string {
	var sb strings.Builder
	// 有结构化字段才写 frontmatter 块 (最小化)
	fieldLines := make([]string, 0, len(fields)+1)
	// title 若存在于 fields 中则其优先; 否则作为单独行?
	// 规范: title 是 frontmatter 的 title 字段 (若存在)
	if t, ok := fields["title"]; ok {
		fieldLines = append(fieldLines, "title: "+yamlQuote(t))
	} else if title != "" {
		fieldLines = append(fieldLines, "title: "+yamlQuote(title))
	}
	// 其余字段按字母序 (稳定)
	for _, k := range sortedKeys(fields) {
		if k == "title" {
			continue
		}
		fieldLines = append(fieldLines, fmt.Sprintf("%s: %s", k, yamlQuote(fields[k])))
	}
	if len(fieldLines) > 0 {
		sb.WriteString("---\n")
		sb.WriteString(strings.Join(fieldLines, "\n"))
		sb.WriteString("\n---\n\n")
	}
	sb.WriteString(body)
	// 确保单文件尾换行
	if !strings.HasSuffix(body, "\n") {
		sb.WriteString("\n")
	}
	return sb.String()
}

// yamlQuote 简单值按需引号 (含特殊字符/数字/布尔时引号, 保证可逆)。
func yamlQuote(s string) string {
	if s == "" {
		return `""`
	}
	needsQuote := strings.ContainsAny(s, ":#{}[],&*!|>'\"%@`\n\t") ||
		strings.TrimSpace(s) != s
	if needsQuote {
		return fmt.Sprintf("%q", s)
	}
	return s
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 简单插入排序即可 (小 map)
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
