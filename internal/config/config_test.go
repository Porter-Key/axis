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

func TestLoad_OutputFormat(t *testing.T) {
	// 默认 json
	cfg, err := Load(filepath.Join(t.TempDir(), "nonexist.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Output.Format != "json" {
		t.Errorf("default output.format=%q, want json", cfg.Output.Format)
	}
	// gcf 生效, 非法值回 json
	for in, want := range map[string]string{"gcf": "gcf", "xml": "json", "": "json"} {
		dir := t.TempDir()
		p := filepath.Join(dir, "c.yaml")
		b, _ := yaml.Marshal(map[string]any{"output": map[string]any{"format": in}})
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Output.Format != want {
			t.Errorf("output.format=%q → %q, want %q", in, cfg.Output.Format, want)
		}
	}
}
