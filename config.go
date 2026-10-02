package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/BurntSushi/toml"
)

const defaultUpstream = "https://api.anthropic.com"

// modelPrefix namespaces the model IDs handed to Claude Code, so the proxy
// can tell cameo subagents apart from real Claude models.
const modelPrefix = "cameo-"

type Config struct {
	// Upstream is where every request that does not belong to a cameo
	// subagent is forwarded, untouched.
	Upstream string            `toml:"upstream"`
	Agents   map[string]*Agent `toml:"agents"`
}

type Agent struct {
	Description string   `toml:"description"`
	Prompt      string   `toml:"prompt"`
	Tools       []string `toml:"tools"`

	// URL is the base URL of an Anthropic-compatible provider, the same value
	// one would put in ANTHROPIC_BASE_URL.
	URL string `toml:"url"`
	// Key may reference environment variables, e.g. "$DEEPSEEK_API_KEY".
	Key   string `toml:"key"`
	Model string `toml:"model"`
}

var agentNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

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
		return nil, fmt.Errorf("%s: unknown key %q", path, keys[0].String())
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

	for _, name := range cfg.agentNames() {
		a := cfg.Agents[name]
		if !agentNameRe.MatchString(name) {
			return nil, fmt.Errorf("%s: invalid agent name %q", path, name)
		}
		a.Key = os.ExpandEnv(a.Key)
		for field, value := range map[string]string{
			"description": a.Description,
			"prompt":      a.Prompt,
			"url":         a.URL,
			"key":         a.Key,
			"model":       a.Model,
		} {
			if value == "" {
				return nil, fmt.Errorf("%s: agents.%s: %s is required", path, name, field)
			}
		}
		if err := checkURL(a.URL); err != nil {
			return nil, fmt.Errorf("%s: agents.%s: url: %w", path, name, err)
		}
	}
	return &cfg, nil
}

func (c *Config) agentNames() []string {
	names := make([]string, 0, len(c.Agents))
	for name := range c.Agents {
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
