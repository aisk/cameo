package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, model, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `
[agents.helper]
description = "d"
prompt = "p"
url = "https://example.com/anthropic"
key = "k"
model = "` + model + `"
` + extra
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestContext1M(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, "third-party-model", "context_1m = true"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Agents["helper"].Model; got != "third-party-model" {
		t.Errorf("Model = %q", got)
	}
	agents, err := agentsJSON(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(agents, `"model":"cameo-helper[1m]"`) {
		t.Errorf("agents = %s", agents)
	}
}

func TestRejects1MSuffixInModel(t *testing.T) {
	_, err := loadConfig(writeConfig(t, "third-party-model[1m]", ""))
	if err == nil || !strings.Contains(err.Error(), "context_1m") {
		t.Errorf("err = %v", err)
	}
}
