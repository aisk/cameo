package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aisk/cameo/llmconv"
)

// keepaliveInterval is how long a streaming client is left without a word
// before it is sent a ping. Reasoning models can stay silent for minutes,
// and Claude Code gives up on a stream that does.
var keepaliveInterval = 15 * time.Second

// maxErrorBody bounds how much of an upstream error reply is read.
const maxErrorBody = 64 << 10

// newUpstreamClient returns the client for translated requests. It has no
// overall timeout, since a reply can stream for many minutes.
func newUpstreamClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
	}}
}

// translate serves a subagent whose provider does not speak Anthropic: the
// request is rebuilt in the provider's protocol and the reply is turned
// back into an Anthropic one.
func (p *Proxy) translate(w http.ResponseWriter, r *http.Request, rt *route, body []byte) {
	switch r.URL.Path {
	case "/v1/messages":
	case "/v1/messages/count_tokens":
		countTokens(w, body)
		return
	default:
		writeError(w, http.StatusNotFound, "not_found_error", "cameo: "+r.URL.Path+" is not available for agent "+rt.agent)
		return
	}

	api := rt.backend.api()
	req, err := llmconv.Parse(llmconv.Anthropic, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cameo: "+err.Error())
		return
	}
	// llmconv reads replies as streams only, so the upstream always
	// streams, whatever the client asked for.
	stream := req.Stream
	req.Stream = true
	out, err := llmconv.Build(api, req, rt.model, llmconv.BuildOptions{Host: rt.backend.host()})
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "cameo: "+err.Error())
		return
	}

	res, err := p.send(r.Context(), rt, out)
	var se *statusError
	if errors.As(err, &se) {
		p.logger.Printf("agent %s: %v", rt.agent, err)
		writeError(w, se.status, errorType(se.status), "cameo: "+se.msg)
		return
	}
	if err != nil {
		p.handleError(w, r, err)
		return
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		p.upstreamError(w, rt, res)
		return
	}
	// Only a JSON reply is surely not a stream: compatible servers are
	// known to stream under another type, or none.
	if mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type")); mt == "application/json" {
		p.logger.Printf("agent %s: upstream answered %q, not an event stream", rt.agent, res.Header.Get("Content-Type"))
		writeError(w, http.StatusBadGateway, "api_error", "cameo: "+rt.provider+" did not answer with an event stream")
		return
	}
	if stream {
		p.streamReply(w, r, rt, res.Body, req)
	} else {
		p.wholeReply(w, rt, res.Body, req)
	}
}

// send makes the upstream request, once more if the backend says a 401 is
// worth another try.
func (p *Proxy) send(ctx context.Context, rt *route, body []byte) (*http.Response, error) {
	for retried := false; ; retried = true {
		req, err := rt.backend.request(ctx, rt.model, body)
		if err != nil {
			return nil, err
		}
		res, err := p.client.Do(req)
		if err != nil {
			return nil, err
		}
		if res.StatusCode != http.StatusUnauthorized || retried || !rt.backend.unauthorized(ctx) {
			return res, nil
		}
		res.Body.Close()
	}
}

// countTokens answers a token count locally, since the other protocols
// have no such endpoint. It is an estimate of about one token per four
// bytes of the request, not a real count.
func countTokens(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"input_tokens": max(1, len(body)/4)})
}

// upstreamError passes an upstream failure on as an Anthropic error of the
// same status, so Claude Code retries what it would retry at Anthropic.
func (p *Proxy) upstreamError(w http.ResponseWriter, rt *route, res *http.Response) {
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBody))
	p.logger.Printf("agent %s: upstream status %d: %s", rt.agent, res.StatusCode, raw)
	status := res.StatusCode
	if status < 400 {
		status = http.StatusBadGateway
	}
	msg := errorMessage(raw, res.Status)
	if _, ok := rt.backend.(*chatgptBackend); ok {
		if more := chatgptExplain(status, raw); more != "" {
			msg += " (" + more + ")"
		}
		if status == http.StatusUnauthorized {
			// Renewing the tokens did not help, or could not be done.
			status = signInStatus
		}
	}
	if v := res.Header.Get("Retry-After"); v != "" {
		w.Header().Set("Retry-After", v)
	}
	writeError(w, status, errorType(status), rt.provider+": "+msg)
}

// errorMessage digs the message out of an error body. OpenAI and Gemini
// both answer {"error": {"message": ...}}, some compatible servers answer
// a bare string or plain text, and OpenAI answers a ChatGPT sign-in at
// times with {"detail": ...}, a text or an object with a message.
func errorMessage(raw []byte, fallback string) string {
	var body struct {
		Error  json.RawMessage `json:"error"`
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(raw, &body) == nil && len(body.Error) > 0 {
		var obj struct {
			Message string `json:"message"`
		}
		var text string
		switch {
		case json.Unmarshal(body.Error, &obj) == nil && obj.Message != "":
			return obj.Message
		case json.Unmarshal(body.Error, &text) == nil && text != "":
			return text
		}
	}
	if len(body.Detail) > 0 {
		var obj struct {
			Message string `json:"message"`
		}
		var text string
		switch {
		case json.Unmarshal(body.Detail, &text) == nil && text != "":
			return text
		case json.Unmarshal(body.Detail, &obj) == nil && obj.Message != "":
			return obj.Message
		}
	}
	if text := strings.TrimSpace(string(raw)); text != "" {
		return text
	}
	return fallback
}

func errorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		return "overloaded_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}

// asClient makes an event name the model the client asked for. The upstream
// names its own, which Claude Code does not know.
func asClient(ev llmconv.Event) llmconv.Event {
	if ev.Kind == llmconv.KStart {
		ev.Model = ""
	}
	return ev
}

// streamReply writes the upstream's event stream to the client as an
// Anthropic one, with a ping whenever the upstream has been silent for
// keepaliveInterval.
func (p *Proxy) streamReply(w http.ResponseWriter, r *http.Request, rt *route, upstream io.Reader, req *llmconv.Request) {
	dec := llmconv.NewDecoder(rt.backend.api())
	enc := llmconv.NewEncoder(llmconv.Anthropic, w, req)

	// The encoder is not safe for concurrent use, so this goroutine only
	// reads and every write happens in the loop below.
	events := make(chan llmconv.Event)
	var readErr error
	go func() {
		defer close(events)
		readErr = llmconv.ReadSSE(upstream, func(_, data string) error {
			return dec.Decode(data, func(ev llmconv.Event) { events <- ev })
		})
	}()

	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()
	started, failed := false, false
	for open := true; open; {
		select {
		case ev, ok := <-events:
			if !ok {
				open = false
				break
			}
			if failed {
				// an error ends the reply: nothing goes on after it
				continue
			}
			if ev.Kind == llmconv.KError {
				failed = true
				p.logger.Printf("agent %s: upstream error in stream: %s", rt.agent, ev.Text)
			}
			enc.Event(asClient(ev))
			started = true
			ticker.Reset(keepaliveInterval)
		case <-ticker.C:
			if failed {
				continue
			}
			if !started {
				// An Anthropic stream opens with message_start, and a
				// ping is only expected after it.
				enc.Event(llmconv.Event{Kind: llmconv.KStart})
				started = true
			}
			enc.Keepalive()
		}
	}

	switch {
	case failed:
	case r.Context().Err() != nil:
		p.logger.Printf("agent %s: client went away", rt.agent)
	case readErr != nil:
		p.logger.Printf("agent %s: reading upstream: %v", rt.agent, readErr)
		enc.Event(llmconv.Event{Kind: llmconv.KError, Text: "cameo: " + readErr.Error()})
	default:
		enc.Finish()
	}
}

// wholeReply collects the upstream's event stream into one Anthropic
// message, for a client that did not ask for a stream.
func (p *Proxy) wholeReply(w http.ResponseWriter, rt *route, upstream io.Reader, req *llmconv.Request) {
	dec := llmconv.NewDecoder(rt.backend.api())
	var col llmconv.Collector
	err := llmconv.ReadSSE(upstream, func(_, data string) error {
		return dec.Decode(data, func(ev llmconv.Event) { col.Add(asClient(ev)) })
	})
	if err != nil {
		p.logger.Printf("agent %s: reading upstream: %v", rt.agent, err)
		writeError(w, http.StatusBadGateway, "api_error", "cameo: "+err.Error())
		return
	}
	res := col.Result()
	// A reply that only thought and then failed is the failure, as
	// llmconv.ConvertResponse has it.
	if err := col.Err(); err != nil && !saidAnything(res.Parts) {
		p.logger.Printf("agent %s: upstream error in stream: %v", rt.agent, err)
		status := http.StatusBadGateway
		var ue *llmconv.UpstreamError
		if errors.As(err, &ue) && ue.Status >= 400 {
			status = ue.Status
		}
		writeError(w, status, errorType(status), rt.provider+": "+err.Error())
		return
	}
	out := llmconv.Render(llmconv.Anthropic, res, req)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.Write(out)
}

func saidAnything(parts []llmconv.Part) bool {
	for _, part := range parts {
		if part.Kind != llmconv.Thinking {
			return true
		}
	}
	return false
}
