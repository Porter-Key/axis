package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLoad_NoFile_Defaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nonexist.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Heartbeat.TimeoutSec != 60 || cfg.Pool.MaxServers != 6 {
		t.Errorf("defaults wrong: %+v %+v", cfg.Heartbeat, cfg.Pool)
	}
	if _, ok := cfg.Adapters["rust"]; !ok {
		t.Error("rust default adapter missing")
	}
}

func TestLoad_OverridesAdapterAndAddsNew(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	// 覆盖 python command, 新增 elixir
	data := map[string]any{
		"adapters": map[string]any{
			"python": map[string]any{"language_id": "python", "command": "/custom/pyright"},
			"elixir": map[string]any{"language_id": "elixir", "command": "elixir-ls"},
		},
		"pool": map[string]any{"max_servers": 3},
	}
	b, _ := yaml.Marshal(data)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Adapters["python"].Command != "/custom/pyright" {
		t.Errorf("python override failed: %+v", cfg.Adapters["python"])
	}
	if _, ok := cfg.Adapters["elixir"]; !ok {
		t.Error("elixir not added")
	}
	if cfg.Pool.MaxServers != 3 {
		t.Errorf("pool override failed: %d", cfg.Pool.MaxServers)
	}
	// 未覆盖的字段保持默认
	if cfg.Adapters["go"].Command != "gopls" {
		t.Errorf("go should keep default: %+v", cfg.Adapters["go"])
	}
}
