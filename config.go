package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

const defaultUpstream = "https://api.anthropic.com"

// modelPrefix namespaces the model IDs handed to Claude Code, so the proxy
// can tell cameo subagents apart from real Claude models.
const modelPrefix = "cameo-"

// context1M is the suffix Claude Code reads off a model ID to give it a 1M
// token context window.
const context1M = "[1m]"

type Config struct {
	// Upstream is where every request that does not belong to a cameo
	// subagent is forwarded, untouched.
	Upstream  string               `toml:"upstream"`
	Providers map[string]*Provider `toml:"providers"`
	Agents    map[string]*Agent    `toml:"agents"`
}

// Provider is one credential at one endpoint, shared by any number of agents.
type Provider struct {
	// API is the wire protocol the endpoint speaks: anthropic (the
	// default), chat, responses or gemini.
	API string `toml:"api"`
	// URL is the base URL, to which cameo appends the endpoint of the API.
	URL string `toml:"url"`
	// Key may reference environment variables, e.g. "$DEEPSEEK_API_KEY".
	Key string `toml:"key"`
}

type Agent struct {
	Description string   `toml:"description"`
	Prompt      string   `toml:"prompt"`
	Tools       []string `toml:"tools"`

	// Provider names the [providers.<name>] table that serves the agent.
	Provider string `toml:"provider"`
	Model    string `toml:"model"`
	// Context1M makes Claude Code treat the model as having a 1M token
	// context window.
	Context1M bool `toml:"context_1m"`
}

// nameRe is what agent and provider names look like.
var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

const defaultAPI = "anthropic"

var apis = []string{"anthropic", "chat", "responses", "gemini"}

// reservedProviders are subscription providers that are built in: signed in
// through cameo, with no key and a fixed endpoint, so they are referenced
// by agents but never defined in the config.
var reservedProviders = []string{chatgptProvider, "antigravity"}

// builtinProviders are the reserved ones that exist already.
var builtinProviders = []string{chatgptProvider}

func configPath() (string, error) {
	if p := os.Getenv("CAMEO_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cameo", "config.toml"), nil
}

func loadConfig(path string) (*Config, error) {
	var cfg Config
	md, err := toml.DecodeFile(path, &cfg)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("config file %s not found, create it or set CAMEO_CONFIG", path)
	}
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		return nil, fmt.Errorf("%s: %w", path, unknownKey(keys[0]))
	}

	if cfg.Upstream == "" {
		cfg.Upstream = os.Getenv("ANTHROPIC_BASE_URL")
	}
	if cfg.Upstream == "" {
		cfg.Upstream = defaultUpstream
	}
	if err := checkURL(cfg.Upstream); err != nil {
		return nil, fmt.Errorf("%s: upstream: %w", path, err)
	}

	for _, name := range sortedKeys(cfg.Providers) {
		if err := checkProvider(name, cfg.Providers[name]); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	for _, name := range sortedKeys(cfg.Agents) {
		if err := cfg.checkAgent(name, cfg.Agents[name]); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return &cfg, nil
}

// unknownKey explains a key the config has no field for. Agents carried
// url and key themselves before providers were split out, so those two get
// a pointer to where they went.
func unknownKey(key toml.Key) error {
	if len(key) == 3 && key[0] == "agents" && (key[2] == "url" || key[2] == "key") {
		return fmt.Errorf("agents.%s: %s is no longer set on an agent, move url and key into a [providers.<name>] table and reference it with provider = \"<name>\"", key[1], key[2])
	}
	return fmt.Errorf("unknown key %q", key.String())
}

func checkProvider(name string, p *Provider) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid provider name %q", name)
	}
	if slices.Contains(reservedProviders, name) {
		return fmt.Errorf("providers.%s: the name %q is reserved for a built-in provider, pick another name", name, name)
	}
	if p.API == "" {
		p.API = defaultAPI
	}
	if !slices.Contains(apis, p.API) {
		return fmt.Errorf("providers.%s: api %q is not one of %s", name, p.API, strings.Join(apis, ", "))
	}
	p.Key = os.ExpandEnv(p.Key)
	if p.URL == "" {
		return fmt.Errorf("providers.%s: url is required", name)
	}
	if p.Key == "" {
		return fmt.Errorf("providers.%s: key is required", name)
	}
	if err := checkURL(p.URL); err != nil {
		return fmt.Errorf("providers.%s: url: %w", name, err)
	}
	return nil
}

func (c *Config) checkAgent(name string, a *Agent) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid agent name %q", name)
	}
	if strings.HasSuffix(a.Model, context1M) {
		return fmt.Errorf("agents.%s: drop %q from model and set context_1m = true instead", name, context1M)
	}
	for _, f := range []struct{ field, value string }{
		{"description", a.Description},
		{"prompt", a.Prompt},
		{"provider", a.Provider},
		{"model", a.Model},
	} {
		if f.value == "" {
			return fmt.Errorf("agents.%s: %s is required", name, f.field)
		}
	}
	if slices.Contains(builtinProviders, a.Provider) {
		return nil
	}
	if slices.Contains(reservedProviders, a.Provider) {
		return fmt.Errorf("agents.%s: provider %q is not supported yet", name, a.Provider)
	}
	if c.Providers[a.Provider] == nil {
		return fmt.Errorf("agents.%s: provider %q is not defined, add a [providers.%s] table", name, a.Provider, a.Provider)
	}
	return nil
}

// checkSignedIn makes sure every built-in provider an agent is served by
// has a sign-in. loadConfig leaves this out, so that the commands that
// sign in can read a config whose agents are waiting for it.
func (c *Config) checkSignedIn(store *authStore) error {
	for _, name := range sortedKeys(c.Agents) {
		provider := c.Agents[name].Provider
		if !slices.Contains(builtinProviders, provider) {
			continue
		}
		in, err := store.load(provider)
		if err != nil {
			return err
		}
		if in == nil {
			return fmt.Errorf("agents.%s: provider %q is not signed in, run: cameo provider login %s", name, provider, provider)
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q is not an http(s) URL", raw)
	}
	return nil
}
