package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/aisk/cameo/llmconv"
)

// route describes where requests for one cameo subagent go.
type route struct {
	agent    string
	provider string
	backend  backend
	// model is the model name at the provider.
	model string
}

type routeKey struct{}

type Proxy struct {
	upstream *url.URL
	// routes is keyed by the model ID Claude Code sends for the subagent.
	routes map[string]*route
	// prefix is a secret path prefix, so other local processes cannot use
	// the proxy to spend the configured provider keys.
	prefix string
	logger *log.Logger
	rp     *httputil.ReverseProxy
	// client sends the translated requests.
	client *http.Client
}

func newProxy(cfg *Config, token string, logger *log.Logger) (*Proxy, error) {
	upstream, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, err
	}
	p := &Proxy{
		upstream: upstream,
		routes:   make(map[string]*route),
		prefix:   "/" + token,
		logger:   logger,
		client:   newUpstreamClient(),
	}
	backends := make(map[string]backend, len(cfg.Providers))
	for name, pr := range cfg.Providers {
		b, err := newBackend(pr)
		if err != nil {
			return nil, fmt.Errorf("providers.%s: %w", name, err)
		}
		backends[name] = b
	}
	for name, a := range cfg.Agents {
		b := backends[a.Provider]
		if b == nil && a.Provider == chatgptProvider {
			store, err := openAuthStore()
			if err != nil {
				return nil, err
			}
			b = newChatGPTBackend(store)
			backends[a.Provider] = b
		}
		if b == nil {
			return nil, fmt.Errorf("agents.%s: provider %q is not defined", name, a.Provider)
		}
		p.routes[modelPrefix+name] = &route{agent: name, provider: a.Provider, backend: b, model: a.Model}
	}
	p.rp = &httputil.ReverseProxy{
		Rewrite:       p.rewrite,
		FlushInterval: -1,
		ErrorLog:      logger,
		ErrorHandler:  p.handleError,
	}
	return p, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path, ok := strings.CutPrefix(r.URL.Path, p.prefix)
	if !ok || (path != "" && path[0] != '/') {
		http.NotFound(w, r)
		return
	}
	r.URL.Path = path
	r.URL.RawPath = ""

	rt, body, err := p.match(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	switch {
	case rt == nil:
		p.logger.Printf("%s %s -> upstream", r.Method, path)
		p.rp.ServeHTTP(w, r)
	case rt.backend.api() == llmconv.Anthropic:
		p.logger.Printf("%s %s -> agent %s (%s)", r.Method, path, rt.agent, rt.model)
		if err := setModel(r, body, rt.model); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		p.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), routeKey{}, rt)))
	default:
		p.logger.Printf("%s %s -> agent %s (%s, %s)", r.Method, path, rt.agent, rt.model, rt.backend.api())
		p.translate(w, r, rt, body)
	}
}

// match reports which subagent a request belongs to, with the body it read
// to find out, or nil if the request should go to the upstream as is.
func (p *Proxy) match(r *http.Request) (*route, []byte, error) {
	if r.Method != http.MethodPost || r.Body == nil || r.Header.Get("Content-Encoding") != "" {
		return nil, nil, nil
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		return nil, nil, nil
	}

	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return nil, nil, err
	}
	setBody(r, body)

	var head struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &head) != nil {
		return nil, nil, nil
	}
	// Claude Code may or may not strip the 1M suffix before sending.
	return p.routes[strings.TrimSuffix(head.Model, context1M)], body, nil
}

// setModel replaces the request body by one asking for the provider's model.
func setModel(r *http.Request, body []byte, model string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return err
	}
	fields["model"], _ = json.Marshal(model)
	body, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	setBody(r, body)
	return nil
}

func setBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
}

func (p *Proxy) rewrite(pr *httputil.ProxyRequest) {
	rt, _ := pr.In.Context().Value(routeKey{}).(*route)
	if rt == nil {
		pr.SetURL(p.upstream)
		return
	}
	// Only a key backend speaks Anthropic, see newBackend.
	kb := rt.backend.(*keyBackend)
	pr.SetURL(kb.url)
	// Never leak the Anthropic credentials to a third party. Providers
	// differ in which header they read, so send the key in both.
	pr.Out.Header.Del("Cookie")
	pr.Out.Header.Set("X-Api-Key", kb.key)
	pr.Out.Header.Set("Authorization", "Bearer "+kb.key)
}

func (p *Proxy) handleError(w http.ResponseWriter, r *http.Request, err error) {
	p.logger.Printf("%s %s: %v", r.Method, r.URL.Path, err)
	writeError(w, http.StatusBadGateway, "api_error", "cameo: "+err.Error())
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": typ, "message": msg},
	})
}
