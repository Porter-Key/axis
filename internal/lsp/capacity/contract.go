// Package lsp_capacity 语言服务器能力契约层。
//
// 分层铁律 (用户要求):
//   - lsp_capacity_contract.go = 唯一必须统一的调用契约: 进出系统的 DTO、能力接口、注册表。
//   - 每个 $language_capacity_provider.go = 完全自包含: 该语言 LSP 的配置真相、进程适配、
//     DTO 特化解析、能力判断全部在文件内, 可接受代码重复, 不接受共享实现造成的耦合。
//   - 任何"看起来像公共工具"的东西 (URI 转换、range 解析、文件级判断) 都不放这里,
//     由各 provider 文件自行内联实现 (即使六份相同)。
package lsp_capacity

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Porter-Key/axis/internal/config"
	"github.com/Porter-Key/axis/internal/lsp/positions"
)

// LanguageID 语言标识 (与配置 adapter key 一致)。
type LanguageID string

const (
	LangGo         LanguageID = "go"
	LangRust       LanguageID = "rust"
	LangCSharp     LanguageID = "csharp"
	LangPython     LanguageID = "python"
	LangTypeScript LanguageID = "typescript"
	LangJavaScript LanguageID = "javascript"
)

// ---- 统一 DTO (进出 lsp_capacity 的边界类型, 契约的一部分) ----

// LocationDTO 统一的位置/符号定位结果 (跨语言归一)。
// 各语言 LSP 返回结构差异在 provider 内部消化, 对外只暴露此 DTO。
type LocationDTO struct {
	URI       string         `json:"uri"`                 // file:// URI
	Path      string         `json:"path,omitempty"`      // 本地绝对路径 (由 provider 转换)
	Range     RangeDTO       `json:"range"`               // 符号范围
	FileLevel bool           `json:"fileLevel,omitempty"` // true=文件/包级跳转 (无符号签名意义)
	Raw       map[string]any `json:"-"`                   // 原始 LSP 项 (签名合并用, 由 provider 填充)
}

// RangeDTO 统一 range (跨语言归一)。
type RangeDTO struct {
	StartLine      int `json:"startLine"`
	StartCharacter int `json:"startCharacter"`
	EndLine        int `json:"endLine"`
	EndCharacter   int `json:"endCharacter"`
}

// PositionDTO 统一位置参数 (跨语言归一, provider 转 LSP 格式)。
type PositionDTO struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Capability 能力枚举 (跨语言统一能力名)。
type Capability string

const (
	CapDefinition      Capability = "definition"
	CapHover           Capability = "hover"
	CapReferences      Capability = "references"
	CapImplementation  Capability = "implementation"
	CapTypeDefinition  Capability = "typeDefinition"
	CapRename          Capability = "rename"
	CapDocumentSymbol  Capability = "documentSymbol"
	CapWorkspaceSymbol Capability = "workspaceSymbol"
	CapCodeAction      Capability = "codeAction"
	CapFormatting      Capability = "formatting"
	CapDiagnostic      Capability = "diagnostic" // LSP 3.17 pull
	CapSignatureHelp   Capability = "signatureHelp"
)

// CapabilityFromString scope 字符串转 Capability (无效/空返回 CapDefinition)。
func CapabilityFromString(s string) Capability {
	switch s {
	case "implementation":
		return CapImplementation
	case "typeDefinition":
		return CapTypeDefinition
	case "hover":
		return CapHover
	default:
		return CapDefinition
	}
}

// ---- 签名 DTO (统一签名结构, 契约的一部分) ----

// SignatureDTO 符号签名结构化块 (统一喂给 LLM 的格式)。
// 由各语言 provider 从自身 hover markdown 形态特化解析产出 (ParseHover),
// 语言差异 (pyright 多行/rust 多围栏/csharp 成员式) 在 provider 文件内消化。
type SignatureDTO struct {
	Symbol    string `json:"symbol"`         // 符号名 (如 (c *Conn).CallWithDoc → CallWithDoc)
	Signature string `json:"signature"`      // 人类可读签名 (可多行, 如 func X(\n a,\n b) R)
	Doc       string `json:"doc,omitempty"`  // 简短文档 (首段)
	Source    string `json:"source"`         // 提取来源: hover (Provider 解析)
	Note      string `json:"note,omitempty"` // 语言特化说明
}

// ---- 进程生命周期句柄 (契约) ----

// ProcessHandle 语言服务器进程的常驻连接句柄 (长连接模型)。由 provider.Spawn 返回。
//
// 长连接模型 (v3 架构, 取代 v2 短连接):
//   - 连接在首次请求时 spawn 并常驻, 不再每请求 didOpen/didClose;
//   - 文件内容一致性由脏标记驱动: 请求某文件前若检测到磁盘内容变更
//     (fsmonitor 或内容 hash 比对), 先 didChange 全量同步再发请求;
//   - 生命周期回收 (空闲 TTL / LRU 上限 / RSS 内存看门狗) 由 registry 负责,
//     registry 只通过本接口操作, 不关心 LSP 协议细节;
//   - 崩溃自愈: IsAlive 探测 + Restart 重启, 指数退避由 registry 管。
type ProcessHandle interface {
	// Pid 进程 ID。
	Pid() int
	// IsAlive 进程是否存活 (registry 崩溃检测轮询用)。
	IsAlive() bool
	// Kill 强制终止进程 (回收/崩溃后清理)。
	Kill() error
	// Close 优雅关闭 (shutdown/exit) 并释放连接 (registry 空闲回收调用)。
	Close() error
	// LastUse 最近使用时间 (registry 空闲 TTL 判断)。
	LastUse() time.Time
	// Touch 标记使用 (registry 每次工具调用后)。
	Touch()

	// ---- 长连接请求 (内容一致性由句柄内部脏标记保证) ----
	// CallWithDoc 在常驻连接上执行一次文件请求:
	//   - 首次请求该文件 → didOpen (读最新盘)
	//   - 后续请求 → 若 NeedsSync 则 didChange 全量同步, 否则直接请求
	//   - 不 didClose (长连接语义)
	CallWithDoc(ctx context.Context, method string, filePath string, extra map[string]any, timeout time.Duration) (json.RawMessage, error)
	// WorkspaceRequest 执行 workspace 级请求 (无需文件)。
	WorkspaceRequest(ctx context.Context, method string, params map[string]any, timeout time.Duration) (json.RawMessage, error)

	// ---- 文件内容同步 (脏标记驱动) ----
	// SyncFile 强制同步文件到服务器 (didChange 全量; 服务器可能据此重新索引/诊断)。
	SyncFile(ctx context.Context, filePath string) error
	// NeedsSync 查询该文件是否需同步 (磁盘内容 hash 与服务器已同步版本不一致)。
	NeedsSync(filePath string) bool
	// Invalidate 标记文件失效 (fsmonitor 报告变更后调用, 下次请求前自动同步)。
	// changedPath 为空 = 整个项目目录失效 (重新索引触发点)。
	Invalidate(changedPath string)

	// PositionEncoding 服务器协商后的位置编码。
	PositionEncoding() string
}

// ---- 位置编码契约 (客户端列单位约定 + 各家自转码) ----

// ClientCharEncoding 客户端列单位: UTF-8 字节偏移 (行内字节数)。
// 所有 MCP 工具的 character 参数与返回位置一律按此口径 (ASCII 下与列号一致;
// 非 ASCII 行按 UTF-8 字节计数)。服务器编码各家不同 (rust-analyzer=utf-8,
// 其余=utf-16, 各家实测见其 provider 文件头注释), 互转由各 provider 自包含实现
// (ServerEncoding/ToServerChar/FromServerChar/AdaptPositionsToClient),
// 宿主与别家文件不猜任何一家的编码。
const ClientCharEncoding = "utf-8"

// ReadLine 读文件指定行 (0-based, 不含换行符), 供 AdaptPositionsToClient 按需取行文本换算列。
// ok=false = 读不到/越界 (该位置跳过换算, 原样保留)。调用方提供带缓存的实现。
// 定义见 positions 包, 此处别名保持契约面稳定。
type ReadLine = positions.ReadLine

// ---- Provider 接口 (契约: 系统唯一调用点) ----

// Provider 一种语言的 LSP 能力提供者。
// 实现必须自包含在单个 *_capacity_provider.go: 配置真相/进程适配/DTO 解析/能力判断
// 全部私有实现于该文件, 不跨文件共享。
type Provider interface {
	// 身份
	LangID() LanguageID
	MatchFile(path string) bool
	Markers() []string
	Adapter() config.LangAdapter // 内置默认配置真相 (可被 config 文件覆盖)

	// 进程生命周期 (契约核心)
	Spawn(ctx context.Context, root string, timeout time.Duration) (ProcessHandle, error)

	// 能力协商 (该语言真实支持, 含奇怪特性)
	Supports(cap Capability) bool

	// DTO 映射 (该语言 LSP 返回 → 统一 DTO; 特化解析在文件内私有实现)
	ParseLocations(raw json.RawMessage) ([]LocationDTO, error)
	PositionParams(p PositionDTO) map[string]any

	// 位置编码 (各适配器自包含: 本家服务器的真实编码 + 双向换算 + 响应适配,
	// 全部私有实现于该文件, 不跨文件共享、不猜别家)。
	// ServerEncoding 本家服务器的位置编码 ("utf-8"/"utf-16"), 必须与实际协商结果一致;
	// 服务器升级编码支持时同步更新本方法 (连带 To/FromServerChar 与 AdaptPositionsToClient)。
	ServerEncoding() string
	// ToServerChar 客户端列 (UTF-8, 见 ClientCharEncoding) → 服务器列。lineText 为目标行文本 (调用方提供)。
	ToServerChar(lineText string, clientChar int) int
	// FromServerChar 服务器列 → 客户端列 (UTF-8)。lineText 为目标行文本。
	FromServerChar(lineText string, serverChar int) int
	// AdaptPositionsToClient 把本家服务器的位置响应换算为客户端单位后返回。
	// 覆盖 Location/LocationLink/DocumentSymbol/SymbolInformation/WorkspaceEdit
	// (单体与数组); 非位置响应、读不到行、越界一律原样保留 (不炸)。
	// srcPath 为查询源文件 (DocumentSymbol 等无 uri 位置的归属; Location 类自带 uri 优先)。
	// 行文本经 readLine 按需获取 (调用方带缓存实现)。
	AdaptPositionsToClient(raw json.RawMessage, srcPath string, readLine ReadLine) json.RawMessage

	// Signature 获取某文件位置符号的标准化签名。
	// 实现自行决策取源与解析策略: 可用 src 发起 hover 查询, hover 不可用或
	// 不完整时自行降级 (如执行文档命令/解析纯文本), 契约不感知任何语言差异。
	// 返回 ok=false 表示该位置无法取得签名 (调用方给出 unavailable 说明)。
	Signature(ctx context.Context, src SignatureSource, path string, line, char int) (SignatureDTO, bool)
}

// SignatureSource 宿主注入的语言服务器查询能力 (通用, 不含语言语义)。
// Provider.Signature 通过它发起查询; 查询失败时的降级策略由实现自己决定。
type SignatureSource interface {
	// HoverMarkdown 对文件某位置发起 hover 查询, 返回服务器 markdown 原文。
	// ok=false = 无 hover 结果或请求失败。
	HoverMarkdown(ctx context.Context, path string, line, char int) (string, bool)
}

// ---- 注册表 (契约: provider 注册与查询的唯一入口) ----

var providers = map[LanguageID]Provider{}

// Register 注册语言 provider (各 *_capacity_provider.go init 调用)。
func Register(p Provider) { providers[p.LangID()] = p }

// Get 按语言取 provider。
func Get(lang LanguageID) (Provider, bool) {
	p, ok := providers[lang]
	return p, ok
}

// All 全部已注册 provider。
func All() []Provider {
	out := make([]Provider, 0, len(providers))
	for _, p := range providers {
		out = append(out, p)
	}
	return out
}
