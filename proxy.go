package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// route describes where requests for one cameo subagent go.
type route struct {
	agent string
	url   *url.URL
	key   string
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
	}
	for name, a := range cfg.Agents {
		u, err := url.Parse(a.URL)
		if err != nil {
			return nil, err
		}
		p.routes[modelPrefix+name] = &route{agent: name, url: u, key: a.Key, model: a.Model}
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

	rt, err := p.match(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if rt != nil {
		p.logger.Printf("%s %s -> agent %s (%s)", r.Method, path, rt.agent, rt.model)
		r = r.WithContext(context.WithValue(r.Context(), routeKey{}, rt))
	} else {
		p.logger.Printf("%s %s -> upstream", r.Method, path)
	}
	p.rp.ServeHTTP(w, r)
}

// match reports which subagent a request belongs to, or nil if it should go
// to the upstream as is. For a matched request the body is replaced by one
// carrying the provider's model name.
func (p *Proxy) match(r *http.Request) (*route, error) {
	if r.Method != http.MethodPost || r.Body == nil || r.Header.Get("Content-Encoding") != "" {
		return nil, nil
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		return nil, nil
	}

	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return nil, err
	}
	setBody(r, body)

	var head struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &head) != nil {
		return nil, nil
	}
	rt := p.routes[head.Model]
	if rt == nil {
		return nil, nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	fields["model"], _ = json.Marshal(rt.model)
	body, err = json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	setBody(r, body)
	return rt, nil
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
	pr.SetURL(rt.url)
	// Never leak the Anthropic credentials to a third party. Providers
	// differ in which header they read, so send the key in both.
	pr.Out.Header.Del("Cookie")
	pr.Out.Header.Set("X-Api-Key", rt.key)
	pr.Out.Header.Set("Authorization", "Bearer "+rt.key)
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
