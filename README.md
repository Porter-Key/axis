# axis

**One resident MCP server. Three precision toolsets. Zero bloat.**

`axis` is a Model Context Protocol (MCP) server that gives coding agents
*convenient, fast access to the tools they already know how to use* — language
semantics via LSP, coarse code-graph exploration, and a project-scoped
knowledge base — behind a single HTTP endpoint.

```
axis (one resident Go binary, ~15 MB RSS idle)
 ├── LSP plugin      → 6 languages, precise symbol semantics
 ├── codegraph plugin→ coarse graph exploration (via CLI)
 ├── memory plugin   → project-scoped markdown knowledge base
 └── gate            → per-session project activation
```

---

## Design philosophy

### MCP is not about giving the agent more tools

MCP's value is not a pile of tools. Piles of tools mean piles of context,
piles of failure modes, and models that don't know which one to pick. The
value of MCP is making the *right* tool **convenient and fast** to use —
so the agent reaches for it instead of grepping blindly.

That drives every decision here:

- **Few, composable tools** — 22 tools total, each with a single obvious job.
  No overlapping "smart" mega-tools.
- **The tools agents already know** — LSP semantics (`get_definition`,
  `get_hover`, `find_callers`...) mirror the IDE features every model has
  seen in training. Zero learning curve.
- **Convenience over ceremony** — activate a project once, then all tools
  just take absolute paths. No registration per file, no project IDs to
  remember.

### Why a resident system service?

LSP servers are expensive to start (gopls: hundreds of ms to seconds) and
hold valuable in-memory state (indexed symbols, dependency graphs). Starting
one per agent request — or per agent session — throws that away and makes
every tool call slow.

A single resident service, shared by *all* agent sessions:

- **LSP processes are spawned once and reused** across sessions and clients,
  with idle TTL + LRU + RSS-watchdog reclamation.
- **File watching is centralized** — one fsnotify stream keeps LSP documents
  and codegraph indexes fresh as you edit.
- **Agents come and go cheaply** — a session is just a heartbeat. The service
  survives, the warm LSPs survive, only the session record is reaped.
- systemd supervises it: crash → restart, stop → whole cgroup (LSP children
  included) physically reaped. No orphans.

### Why Go?

- **One static binary, no runtime.** No Node, no Python, no JVM to install,
  version-pin, or let rot. `go build` → copy → run.
- **Tiny resident footprint.** The whole server (MCP + LSP orchestration +
  SQLite knowledge base + fsmonitor) idles at **~15 MB RSS** — less than one
  open browser tab, smaller than most single LSP servers.
- **First-class concurrency** for the hard parts: framing LSP
  JSON-RPC/Content-Length streams, per-language goroutines, bounded pools.
- **Single deployable.** An agent runtime could even embed it as a library;
  the process boundary stays optional.

### Why memory is designed this way (md files as source of truth)

Memory that dies with its tool is not memory. Prior agent-memory tools
locked knowledge in private formats or server-side databases that were
painful to read, grep, or migrate away from.

axis memory inverts that:

- **Markdown files ARE the memory.** Each note lives at
  `<project>/.axis/<category>/<name>.md` — a plain file inside the project.
  You can read it, edit it with any tool, grep it, version it with git,
  carry it when you move the project.
- **The database is just an index.** SQLite is derived from those files for
  fast full-text search and structured fields. `mem_update` reads the file
  back into the index — the file is authoritative, the DB can always be
  rebuilt.
- **No wiki-link tax.** Links live only in the DB (`mem_link`), never as
  fragile `[[...]]` syntax in text. Notes stay clean; relations never rot.
- **Project-scoped by default.** Each project has its own isolated DB —
  no cross-project contamination, no namespacing ceremony. Global knowledge
  is explicit (`project="global"`).
- **Written with tools the agent already has.** Persist = plain
  `write`/`edit` on a markdown file. Index = one no-argument `mem_update`.

### Why an activation gate (not auto-discovery)?

A resident service shared by many projects needs to know *which project you
mean* — but guessing from the current working directory is exactly how notes
and LSP roots land in the wrong place.

So the agent states it explicitly once:

```
axis_activate(project=/abs/path/to/project)
```

Directory-level tools are rejected until then. Switching projects is an
explicit re-activate. The cost is one call; the benefit is that cross-project
mistakes (wrong memory, wrong index, wrong LSP root) become impossible.

---

## Features

- **Six languages via LSP**, config-driven (hot-reloadable):
  Go (gopls) · Rust (rust-analyzer) · C# (csharp-ls) · Python (pyright) ·
  TypeScript/JavaScript (typescript-language-server)
- **Precise tools**: definition / implementation / typeDefinition / hover /
  references / rename impact / symbols / diagnostics
- **Coarse graph exploration** via `codegraph`: explore / query / callers /
  callees / impact — where precise LSP is overkill, graph search wins
- **Project knowledge base**: full-text searchable, markdown-backed,
  per-project isolated memory
- **Lazy LSP spawn + idle TTL + LRU cap + RSS watchdog**: memory stays flat
- **fsnotify file watching**: dirty marks → `didChange` sync; no stale docs
- **Session registration/heartbeat**: agents register, heartbeat, vanish —
  service keeps the warm state
- **stdio mode** for embedding, **HTTP mode** for a shared resident service

## Quick start

```bash
# build
go build -o axis ./cmd/axis

# stdio mode (run as an MCP child process)
./axis

# HTTP resident mode (default 127.0.0.1:1940)
./axis -http -addr 127.0.0.1:1940 -config configs/axis.yaml
```

Requires the LSP binaries of the languages you use (see
[`configs/axis.yaml`](configs/axis.yaml)):
`gopls`, `rust-analyzer`, `csharp-ls`, `pyright-langserver`,
`typescript-language-server`.

## Configuration

See [`configs/axis.yaml`](configs/axis.yaml) — the example ships with every
option commented. Load order: `-config` flag → `./configs/axis.yaml` →
`./axis.yaml` → `~/.config/mcp/axis/config.yaml` → built-in defaults.
Hot reload: `POST /ctrl/reload` (when a `ctrl_token` is configured) or
restart.

## Connecting from an agent (opencode example)

```jsonc
// opencode.json
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

Then, in the session, before using directory-level tools:

```
axis_activate(project=/abs/path/to/your/project)
```

That's it. Detailed setup (systemd, session lifecycle, memory workflow):
[`docs/setup.zh.md`](docs/setup.zh.md).

## MCP tools (22)

| Tool | Plugin | Purpose |
|---|---|---|
| `axis_activate` | gate | Bind this session to a project (required first) |
| `get_definition` | LSP | Definition / implementation / typeDefinition locations |
| `get_hover` | LSP | Type & doc at position |
| `get_references` | LSP | Project references (with signatures) |
| `get_rename` | LSP | Rename impact surface |
| `get_symbols` | LSP | File / workspace symbols |
| `get_diagnostics` | LSP | Compiler errors & warnings |
| `explore_code` | codegraph | Task → relevant symbols + call paths |
| `query_symbols` | codegraph | Fuzzy symbol search (JSON) |
| `find_callers` / `find_callees` | codegraph | Call graph directions |
| `analyze_impact` | codegraph | Blast radius of a symbol change |
| `index_status` / `list_files` | codegraph | Index health / coverage |
| `mem_find` | memory | Full-text search project notes |
| `mem_retrieve` | memory | Read a note (fields/links) |
| `mem_update` | memory | Index a `.axis/...md` file into DB |
| `mem_list` / `mem_status` | memory | Note inventory / sync state |
| `mem_link` / `mem_field` | memory | N:N relations / structured fields |
| `mem_export` | memory | DB → markdown export |

All LSP tools take absolute `path` (+ `line`/`character`). Language is
detected from the file extension; the project root is found by walking up to
a marker file (go.mod, Cargo.toml, ...).

## HTTP control plane (`/ctrl/`)

For lifecycle integrations (e.g. an agent plugin that registers/heartbeats):

| Endpoint | Purpose |
|---|---|
| `POST /ctrl/register` | `{"token","project","user_agent"}` — register a session |
| `GET /ctrl/heartbeat?token=` | keep session alive |
| `GET /ctrl/unregister?token=` | release session on client exit |
| `GET /ctrl/activate?project=` | pre-warm a project |
| `POST /ctrl/reload` | hot-reload config |
| `GET /ctrl/status` | full state (projects/sessions/pool/memory) |

> Bind to 127.0.0.1 and leave `ctrl_token` empty for local use. If exposed
> beyond loopback, set a token.

## systemd

Example unit in [`configs/axis.service`](configs/axis.service) (customize
paths/user/PATH, then `sudo systemctl enable --now axis`).

## Tests

```bash
go test ./...    # unit + real-LSP integration (auto-skips if missing)
go vet ./...
```

## License

[MIT](LICENSE)
