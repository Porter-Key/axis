package axis

import (
	"encoding/json"
	"net/http"
	"path/filepath"

	"github.com/Porter-Key/axis/internal/config"
)

// ---------- 控制面 HTTP ----------

// RegisterCtrlHTTP 把控制面路由挂到 mux (/ctrl/ 前缀)。
func (a *App) RegisterCtrlHTTP(mux *http.ServeMux, prefix string) {
	mux.HandleFunc(prefix+"register", a.ctrlRegister)
	mux.HandleFunc(prefix+"heartbeat", a.ctrlHeartbeat)
	mux.HandleFunc(prefix+"unregister", a.ctrlUnregister)
	mux.HandleFunc(prefix+"activate", a.ctrlActivate)
	mux.HandleFunc(prefix+"reload", a.ctrlReload)
	mux.HandleFunc(prefix+"status", a.ctrlStatus)
}

func (a *App) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *App) ctrlRegister(w http.ResponseWriter, r *http.Request) {
	if !a.auth(w, r) {
		return
	}
	var body struct {
		Token   string `json:"token"`
		Project string `json:"project"`
		UA      string `json:"user_agent"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Token == "" || body.Project == "" {
		a.writeJSON(w, 400, map[string]any{"error": "token/project 必填"})
		return
	}
	root, _ := filepath.Abs(body.Project)
	a.lsp.RegisterProject(root)
	a.lsp.StartMonitor(root)
	if err := a.lsp.RegisterSession(body.Token, root, body.UA); err != nil {
		a.writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	a.writeJSON(w, 200, map[string]any{"ok": true, "project": root})
}

func (a *App) ctrlHeartbeat(w http.ResponseWriter, r *http.Request) {
	if !a.auth(w, r) {
		return
	}
	token := r.URL.Query().Get("token")
	if !a.lsp.Heartbeat(token) {
		a.writeJSON(w, 404, map[string]any{"error": "session 不存在"})
		return
	}
	a.writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) ctrlUnregister(w http.ResponseWriter, r *http.Request) {
	if !a.auth(w, r) {
		return
	}
	token := r.URL.Query().Get("token")
	a.lsp.UnregisterSession(token)
	a.writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) ctrlActivate(w http.ResponseWriter, r *http.Request) {
	if !a.auth(w, r) {
		return
	}
	// 预热: 项目下各语言的 LSP 提前拉起
	root := r.URL.Query().Get("project")
	abs, _ := filepath.Abs(root)
	a.lsp.RegisterProject(abs)
	a.lsp.StartMonitor(abs)
	a.writeJSON(w, 200, map[string]any{"ok": true, "project": abs, "note": "懒加载: 首个工具调用时才真正 spawn LSP"})
}

func (a *App) ctrlReload(w http.ResponseWriter, r *http.Request) {
	if !a.auth(w, r) {
		return
	}
	// 热重载配置: 用启动时的同一路径重新加载 (adapter/pool/超时/codegraph 即时生效;
	// memory 库目录变更需重启, 这里只读不碰)。
	newCfg, err := config.Load(a.cfgPath)
	if err != nil {
		a.writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	a.cfg.Store(newCfg)
	a.lsp.SetConfig(newCfg) // 透传 registry (不透传等于没重载)
	a.cg.SetConfig(newCfg.Codegraph)
	a.cg.SetOutputFormat(newCfg.Output.Format)  // codegraph GCF 开关热生效
	a.mem.SetOutputFormat(newCfg.Output.Format) // memory GCF 开关热生效
	a.writeJSON(w, 200, map[string]any{"ok": true, "adapters": langsList(newCfg),
		"config": config.ResolvePath(a.cfgPath), "note": "memory 库目录变更需重启生效"})
}

func (a *App) ctrlStatus(w http.ResponseWriter, r *http.Request) {
	if !a.auth(w, r) {
		return
	}
	a.writeJSON(w, 200, a.lsp.Status())
}

func (a *App) auth(w http.ResponseWriter, r *http.Request) bool {
	if a.cfg.Load().CtrlToken == "" {
		return true // 无 token 则信任 (localhost)
	}
	tok := r.Header.Get("X-Ctrl-Token")
	if tok != a.cfg.Load().CtrlToken {
		a.writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return false
	}
	return true
}
