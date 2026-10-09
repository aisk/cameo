package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testProvider = `
[providers.third]
url = "https://example.com/anthropic"
key = "k"
`

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeConfig(t *testing.T, model, extra string) string {
	t.Helper()
	return writeFile(t, testProvider+`
[agents.helper]
description = "d"
prompt = "p"
provider = "third"
model = "`+model+`"
`+extra)
}

// agent is the text of an [agents.helper] table served by provider.
func agent(provider string) string {
	return `
[agents.helper]
description = "d"
prompt = "p"
provider = "` + provider + `"
model = "m"
`
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

func TestProviders(t *testing.T) {
	t.Setenv("CAMEO_TEST_KEY", "from-env")
	cfg, err := loadConfig(writeFile(t, `
[providers.third]
url = "https://example.com/anthropic"
key = "$CAMEO_TEST_KEY"

[providers.openai]
api = "responses"
url = "https://api.openai.com/v1"
key = "sk-literal"
`+agent("openai")))
	if err != nil {
		t.Fatal(err)
	}
	third, openai := cfg.Providers["third"], cfg.Providers["openai"]
	if third.API != "anthropic" {
		t.Errorf("default api = %q", third.API)
	}
	if third.Key != "from-env" {
		t.Errorf("key = %q, want it expanded", third.Key)
	}
	if openai.API != "responses" || openai.URL != "https://api.openai.com/v1" || openai.Key != "sk-literal" {
		t.Errorf("openai = %+v", openai)
	}
	if got := cfg.Agents["helper"].Provider; got != "openai" {
		t.Errorf("Provider = %q", got)
	}
}

func TestConfigErrors(t *testing.T) {
	t.Setenv("CAMEO_TEST_UNSET", "")
	provider := func(name, body string) string { return "[providers." + name + "]\n" + body + "\n" }
	keyed := `url = "https://example.com/v1"` + "\n" + `key = "k"`
	for name, tc := range map[string]struct {
		config string
		want   []string
	}{
		"bad api": {
			provider("p", `api = "openai"`+"\n"+keyed) + agent("p"),
			[]string{"providers.p", `"openai"`, "anthropic, chat, responses, gemini"},
		},
		"undefined provider": {
			testProvider + agent("nowhere"),
			[]string{"agents.helper", `"nowhere"`, "not defined"},
		},
		"missing provider": {
			testProvider + "[agents.helper]\n" + `description = "d"` + "\n" + `prompt = "p"` + "\n" + `model = "m"`,
			[]string{"agents.helper", "provider is required"},
		},
		"reserved codex": {
			provider("codex", keyed),
			[]string{"providers.codex", "reserved"},
		},
		"reserved antigravity": {
			provider("antigravity", keyed),
			[]string{"providers.antigravity", "reserved"},
		},
		"agent on codex": {
			agent("codex"),
			[]string{"agents.helper", `"codex"`, "not supported yet"},
		},
		"agent on antigravity": {
			agent("antigravity"),
			[]string{"agents.helper", `"antigravity"`, "not supported yet"},
		},
		"agent url": {
			testProvider + agent("third") + `url = "https://example.com/anthropic"`,
			[]string{"agents.helper", "url", "[providers.<name>]", `provider = "<name>"`},
		},
		"agent key": {
			testProvider + agent("third") + `key = "k"`,
			[]string{"agents.helper", "key", "[providers.<name>]", `provider = "<name>"`},
		},
		"unknown key": {
			testProvider + agent("third") + `colour = "red"`,
			[]string{"unknown key", "agents.helper.colour"},
		},
		"missing url": {
			provider("p", `key = "k"`) + agent("p"),
			[]string{"providers.p", "url is required"},
		},
		"empty key": {
			provider("p", `url = "https://example.com/v1"`+"\n"+`key = "$CAMEO_TEST_UNSET"`) + agent("p"),
			[]string{"providers.p", "key is required"},
		},
		"bad url": {
			provider("p", `url = "example.com"`+"\n"+`key = "k"`) + agent("p"),
			[]string{"providers.p", "url"},
		},
		"bad provider name": {
			provider(`"a b"`, keyed),
			[]string{"invalid provider name"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadConfig(writeFile(t, tc.config))
			if err == nil {
				t.Fatal("no error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to mention %q", err, want)
				}
			}
		})
	}
}

func TestExampleConfig(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "a")
	t.Setenv("GLM_API_KEY", "b")
	if _, err := loadConfig("config.example.toml"); err != nil {
		t.Error(err)
	}
}
