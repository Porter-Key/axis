# axis 配置与使用指南（中文）

> axis 是一个常驻 MCP server：多语言 LSP 语义工具 + codegraph 图谱探索 + 项目知识库（memory），
> 一个 HTTP 端点统一提供。本文覆盖安装、接入、激活门禁、memory 工作流与配置详解。

## 1. 构建与启动

```bash
go build -o axis ./cmd/axis

# stdio 模式（作为 MCP 客户端子进程，单会话）
./axis

# HTTP 常驻模式（多会话共享，推荐；默认 127.0.0.1:1940）
./axis -http -addr 127.0.0.1:1940 -config configs/axis.yaml
```

### systemd 常驻（推荐）

1. 编辑 `configs/axis.service`：改 `User`、`ExecStart` 二进制路径、`PATH`
   （必须包含你所用全部 LSP 的目录）。
2. 安装并启动：

```bash
sudo cp configs/axis.service /etc/systemd/system/axis.service
sudo systemctl daemon-reload && sudo systemctl enable --now axis
systemctl status axis   # 应 active
curl -s http://127.0.0.1:1940/ctrl/status   # 控制面可见
```

LSP 子进程与 axis 同 cgroup，`systemctl stop axis` 会连锅端回收，无残留。

## 2. 前置：语言服务器

每种想用的语言需已安装对应 LSP，且可在 `configs/axis.yaml` 的 `adapters`
中通过绝对路径或 PATH 找到：

| 语言 | LSP | 安装示例 |
|---|---|---|
| Go | gopls | `go install golang.org/x/tools/gopls@latest` |
| Rust | rust-analyzer | `rustup component add rust-analyzer` |
| C# | csharp-ls | `dotnet tool install -g csharp-ls` |
| Python | pyright | `npm i -g pyright` |
| TypeScript / JavaScript | typescript-language-server | `npm i -g typescript-language-server typescript` |

## 3. 接入 opencode（示例）

```jsonc
// opencode.json（或 ~/.config/opencode/opencode.json）
{
  "mcp": {
    "axis": {
      "type": "remote",
      "url": "http://127.0.0.1:1940/mcp",
      "enabled": true
    }
  }
}
```

## 4. 激活门禁（重要）

axis 常驻服务被多个项目共享。**目录级工具（LSP / codegraph / memory）必须先
把当前会话绑定到一个项目**，否则一律拒绝：

```
axis_activate(project=/绝对/路径/到/项目根)
```

规则：

- **换目录/换项目必须重新 `axis_activate`**；严格绑定，不能跨项目操作。
- 未激活 → 目录级工具全部报错（防串项目、防写错库）。
- 记忆按项目隔离（每项目独立 DB）；全局知识需显式 `project="global"`。

## 5. memory 工作流（项目知识库）

设计核心：**md 文件即真相，DB 只是索引**。笔记就是项目内普通文件。

### 写入（操作完成后）

1. 在项目内落盘：`<项目>/.axis/<分类>/<名字>.md`（用任意编辑器/write 工具）。
2. 入库（无参数，服务端读该文件）：

```
mem_update(key="分类/名字")          # 例如 key="arch/decision-2026"
```

### 检索（操作开始前）

```
mem_find(query="关键词")             # 全文检索当前项目笔记（中文友好）
mem_retrieve(key="分类/名字")        # 读完整笔记（含出链/反链/字段）
```

全局知识：`mem_find(query=..., project="global")`。

### 链接与字段

md 文本**没有**链接语法（不要写 `[[...]]`，那只产生提示）。关系是 N:N、
只存 DB：

```
mem_link(src=key, dst=key, type=relates_to)   # 建关联
mem_field(key=..., name=tags, value=...)       # 维护结构化字段
```

### 导出

```
mem_export(key=...)   # 单篇；不带 key = 全量导出 md 到 .axis/
```

## 6. 配置详解（configs/axis.yaml）

- `adapters.<lang>.command/args`：语言 → LSP 命令。绝对路径最稳（systemd
  环境 PATH 受限）；缺省用内置默认（相对名走 PATH）。
- `adapters.<lang>.timeout_sec`：该语言请求超时（如 C# 冷启动慢可设 60）。
- `heartbeat.timeout_sec/interval_sec`：会话心跳超时与建议间隔。
- `pool.idle_ttl_sec`：LSP 空闲回收（默认 900s）。
- `pool.max_servers`：同时 spawn 的 LSP 上限（LRU，默认 6）。
- `pool.memory_limit_mb`：池总 RSS 看门狗（超限先回收空闲，默认 1024）。
- `codegraph.bin/auto_init/sync_on_change/timeout_sec`：图谱 CLI 集成。
- `ignore`：fsmonitor/codegraph 通用忽略（.git/node_modules/target/...）。
- `memory.db_path/export_dir/extension`：memory 索引与导出位置
  （默认 DB 在 `~/.local/state/axis/memdb/<项目>.db`，导出在项目 `.axis/`）。
- `log.path/level`：结构化 JSONL 日志。
- `ctrl_token`：控制面口令（仅绑 127.0.0.1 时可留空）。

热重载：`POST /ctrl/reload`（改 adapters 无需重启进程）。

## 7. 控制面（/ctrl/）

供生命周期集成（如 agent 插件注册/心跳）：

| 端点 | 说明 |
|---|---|
| `POST /ctrl/register` | `{"token","project","user_agent"}` 注册会话 |
| `GET /ctrl/heartbeat?token=` | 心跳续期 |
| `GET /ctrl/unregister?token=` | 注销会话 |
| `GET /ctrl/activate?project=` | 预热项目（提前拉起 LSP） |
| `POST /ctrl/reload` | 热重载配置 |
| `GET /ctrl/status` | 全状态（项目/会话/LSP 池/内存） |

> 仅本机使用可绑 127.0.0.1 且不设 token；对外暴露必须设 `ctrl_token`。

## 8. 验证

```bash
curl -s http://127.0.0.1:1940/ctrl/status
# 应看到 projects / sessions / lsp_pool / memory 状态
```

## 9. 测试

```bash
go test ./...    # 含真实 LSP 集成测试（缺环境自动跳过）
go vet ./...
```
