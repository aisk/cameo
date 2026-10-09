package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type captured struct {
	path   string
	query  string
	header http.Header
	body   string
}

func capture(t *testing.T, got *captured) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*got = captured{path: r.URL.Path, query: r.URL.RawQuery, header: r.Header, body: string(body)}
		io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setup(t *testing.T) (proxyURL string, upstream, provider *captured) {
	t.Helper()
	upstream, provider = new(captured), new(captured)
	cfg := &Config{
		Upstream: capture(t, upstream).URL,
		Providers: map[string]*Provider{
			"third": {API: "anthropic", URL: capture(t, provider).URL + "/anthropic", Key: "provider-key"},
		},
		Agents: map[string]*Agent{
			"helper": {Provider: "third", Model: "third-party-model"},
		},
	}
	p, err := newProxy(cfg, "secret", log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv.URL, upstream, provider
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer claude-token")
	req.Header.Set("Cookie", "session=claude")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestClaudeModelPassesThrough(t *testing.T) {
	proxyURL, upstream, provider := setup(t)
	body := `{"model": "claude-opus-5-5",  "max_tokens": 1}`
	post(t, proxyURL+"/secret/v1/messages?beta=true", body)

	if provider.path != "" {
		t.Fatalf("provider was called: %+v", provider)
	}
	if upstream.path != "/v1/messages" || upstream.query != "beta=true" {
		t.Errorf("upstream got %s?%s", upstream.path, upstream.query)
	}
	if upstream.body != body {
		t.Errorf("body was modified: %s", upstream.body)
	}
	if got := upstream.header.Get("Authorization"); got != "Bearer claude-token" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestAgentModelGoesToProvider(t *testing.T) {
	proxyURL, upstream, provider := setup(t)
	post(t, proxyURL+"/secret/v1/messages?beta=true", `{"model":"cameo-helper","max_tokens":1}`)

	if upstream.path != "" {
		t.Fatalf("upstream was called: %+v", upstream)
	}
	if provider.path != "/anthropic/v1/messages" || provider.query != "beta=true" {
		t.Errorf("provider got %s?%s", provider.path, provider.query)
	}
	var sent struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	if err := json.Unmarshal([]byte(provider.body), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Model != "third-party-model" || sent.MaxTokens != 1 {
		t.Errorf("body = %s", provider.body)
	}
	if got := provider.header.Get("Authorization"); got != "Bearer provider-key" {
		t.Errorf("Authorization = %q", got)
	}
	if got := provider.header.Get("X-Api-Key"); got != "provider-key" {
		t.Errorf("X-Api-Key = %q", got)
	}
	if got := provider.header.Get("Cookie"); got != "" {
		t.Errorf("Cookie = %q", got)
	}
	// An Anthropic-compatible provider still gets the client's own headers.
	if got := provider.header.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Errorf("Anthropic-Version = %q", got)
	}
}

func TestAgentModelWith1MSuffix(t *testing.T) {
	proxyURL, upstream, provider := setup(t)
	post(t, proxyURL+"/secret/v1/messages", `{"model":"cameo-helper[1m]","max_tokens":1}`)

	if upstream.path != "" {
		t.Fatalf("upstream was called: %+v", upstream)
	}
	if !strings.Contains(provider.body, `"model":"third-party-model"`) {
		t.Errorf("body = %s", provider.body)
	}
}

func TestRejectsMissingToken(t *testing.T) {
	proxyURL, upstream, provider := setup(t)
	for _, path := range []string{"/v1/messages", "/secretx/v1/messages"} {
		resp := post(t, proxyURL+path, `{"model":"cameo-helper"}`)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d", path, resp.StatusCode)
		}
	}
	if upstream.path != "" || provider.path != "" {
		t.Error("request was forwarded")
	}
}

func TestUnknownProviderInRoute(t *testing.T) {
	cfg := &Config{
		Upstream: "https://api.anthropic.com",
		Agents:   map[string]*Agent{"helper": {Provider: "missing", Model: "m"}},
	}
	if _, err := newProxy(cfg, "secret", log.New(io.Discard, "", 0)); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("err = %v", err)
	}
}
