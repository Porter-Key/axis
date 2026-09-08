// Package langs 根据文件路径检测语言与项目根。
// 项目根由 marker 文件 (go.mod / Cargo.toml 等) 向上回溯确定。
package langs

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/Porter-Key/axis/internal/config"
)

// Detect 返回文件的 language_id (基于扩展名匹配 adapters 的 file_patterns)。
func Detect(cfg *config.Config, path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return ""
	}
	for lang, ad := range cfg.Adapters {
		for _, p := range ad.FilePatterns {
			// 简化 glob: **/*.go -> *.go 后缀匹配
			pat := strings.TrimPrefix(p, "**/*")
			if pat != "" && strings.HasSuffix(path, pat) {
				_ = lang
				_ = ad
				_ = p
				_ = ext
				// 精确后缀匹配 (比 glob 可靠)
			}
		}
	}
	// 精确后缀匹配 (glob 简化为后缀判断)
	switch ext {
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	case ".cs":
		return "csharp"
	case ".py":
		return "python"
	case ".ts", ".tsx", ".mts", ".cts":
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	}
	// 兜底: 用 adapters 的 file_patterns 后缀
	for lang, ad := range cfg.Adapters {
		for _, p := range ad.FilePatterns {
			pat := strings.TrimPrefix(p, "**/*")
			if pat != "" && strings.HasSuffix(path, pat) {
				return lang
			}
		}
	}
	return ""
}

// FindProjectRoot 从文件路径向上回溯找项目根 (含任一 marker 的目录)。
// 返回根目录与找到的 marker。找不到返回 false。
func FindProjectRoot(cfg *config.Config, lang, filePath string) (root string, marker string, ok bool) {
	dir := filepath.Dir(filePath)
	ad, exists := cfg.Adapters[lang]
	if !exists {
		return "", "", false
	}
	for {
		for _, m := range ad.Markers {
			// marker 支持 glob (如 *.csproj), 需目录扫描
			if strings.ContainsAny(m, "*?[") {
				matches, _ := filepath.Glob(filepath.Join(dir, m))
				if len(matches) > 0 {
					return dir, filepath.Base(matches[0]), true
				}
			} else if isFile(filepath.Join(dir, m)) {
				return dir, m, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", "", false
}

// LookupAdapter 返回语言对应的适配器。
func LookupAdapter(cfg *config.Config, lang string) (config.LangAdapter, bool) {
	ad, ok := cfg.Adapters[lang]
	return ad, ok
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
