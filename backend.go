package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/aisk/cameo/llmconv"
)

// backend is the provider side of a route: a credential and the endpoint
// it is good for.
type backend interface {
	// api is the protocol the provider speaks.
	api() llmconv.Protocol
	// host is the provider's host name, by which llmconv recognises the
	// quirks of a vendor.
	host() string
	// request builds the upstream request for a body already converted to
	// api, asking for model.
	request(ctx context.Context, model string, body []byte) (*http.Request, error)
	// unauthorized is called when the upstream answers 401. It reports
	// whether the request is worth sending once more, which is how a
	// backend with a token that expires gets to refresh it.
	unauthorized(ctx context.Context) bool
}

func newBackend(p *Provider) (backend, error) {
	u, err := url.Parse(p.URL)
	if err != nil {
		return nil, err
	}
	proto, ok := protocols[p.API]
	if !ok {
		return nil, fmt.Errorf("unknown api %q", p.API)
	}
	return &keyBackend{proto: proto, url: u, key: p.Key}, nil
}

var protocols = map[string]llmconv.Protocol{
	"anthropic": llmconv.Anthropic,
	"chat":      llmconv.Chat,
	"responses": llmconv.Responses,
	"gemini":    llmconv.Gemini,
}

// keyBackend is a provider reached with a static API key.
type keyBackend struct {
	proto llmconv.Protocol
	url   *url.URL
	key   string
}

func (b *keyBackend) api() llmconv.Protocol { return b.proto }

func (b *keyBackend) host() string { return b.url.Hostname() }

// request is only used for the translated protocols. An Anthropic provider
// is served by the reverse proxy, which keeps the client's path and headers.
func (b *keyBackend) request(ctx context.Context, model string, body []byte) (*http.Request, error) {
	u := *b.url
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath, u.RawQuery = "", ""
	header := http.Header{}
	switch b.proto {
	case llmconv.Chat:
		u.Path += "/chat/completions"
		header.Set("Authorization", "Bearer "+b.key)
	case llmconv.Responses:
		u.Path += "/responses"
		header.Set("Authorization", "Bearer "+b.key)
	case llmconv.Gemini:
		// Gemini names the model and asks for a stream in the URL, and
		// Google refuses a body that carries a model field.
		u.Path += "/models/" + model + ":streamGenerateContent"
		u.RawQuery = "alt=sse"
		header.Set("X-Goog-Api-Key", b.key)
		body = withoutField(body, "model")
	default:
		return nil, fmt.Errorf("no translated endpoint for api %q", b.proto)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = header
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	return req, nil
}

// unauthorized has nothing to try again with: a key is right or it is not.
func (b *keyBackend) unauthorized(context.Context) bool { return false }

// withoutField drops one top-level field of a JSON object.
func withoutField(body []byte, field string) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return body
	}
	if _, ok := fields[field]; !ok {
		return body
	}
	delete(fields, field)
	out, err := json.Marshal(fields)
	if err != nil {
		return body
	}
	return out
}
