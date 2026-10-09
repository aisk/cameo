package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

const messagesRequest = `{
  "model": "cameo-helper",
  "max_tokens": 1024,
  "stream": true,
  "system": "You are a weather assistant.",
  "tools": [{
    "name": "get_weather",
    "description": "Look up the weather in a city.",
    "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}
  }],
  "messages": [{"role": "user", "content": "What is the weather in Paris?"}]
}`

// A Chat Completions stream: text, then one tool call whose arguments
// arrive in two pieces, then the usage.
const chatReply = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"provider-model","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"provider-model","choices":[{"index":0,"delta":{"content":"Let me "},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"provider-model","choices":[{"index":0,"delta":{"content":"check."},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"provider-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"provider-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"provider-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"provider-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"provider-model","choices":[],"usage":{"prompt_tokens":25,"completion_tokens":12,"total_tokens":37}}

data: [DONE]

`

const chatHello = `data: {"id":"chatcmpl-2","model":"provider-model","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-2","model":"provider-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

const responsesReply = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"provider-model","status":"in_progress"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":" there"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"provider-model","status":"completed","usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}}

`

const geminiReply = `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]}}],"modelVersion":"provider-model"}

data: {"candidates":[{"content":{"role":"model","parts":[{"text":" there"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9}}

`

// lockedBuffer is a log sink the proxy's goroutines can share with a test.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type translated struct {
	url   string // of the proxy, with its secret prefix
	proxy *Proxy
	logs  *lockedBuffer
}

// translate starts a proxy with one agent, "helper", served by a fake
// provider that speaks api and runs handler.
func translate(t *testing.T, api string, handler http.HandlerFunc) *translated {
	t.Helper()
	provider := httptest.NewServer(handler)
	t.Cleanup(provider.Close)
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Anthropic upstream was called: %s %s", r.Method, r.URL)
	}))
	t.Cleanup(anthropic.Close)

	cfg := &Config{
		Upstream:  anthropic.URL,
		Providers: map[string]*Provider{"third": {API: api, URL: provider.URL + "/base/", Key: "provider-key"}},
		Agents:    map[string]*Agent{"helper": {Provider: "third", Model: "provider-model"}},
	}
	logs := new(lockedBuffer)
	p, err := newProxy(cfg, "secret", log.New(logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return &translated{url: srv.URL + "/secret", proxy: p, logs: logs}
}

// sse answers with body as an event stream.
func sse(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/event-stream")
	io.WriteString(w, body)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// record keeps what the provider was sent and answers with reply.
func record(got *captured, reply string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*got = captured{path: r.URL.Path, query: r.URL.RawQuery, header: r.Header, body: string(body)}
		sse(w, reply)
	}
}

// ask posts body the way Claude Code would, with its Anthropic credentials.
func ask(t *testing.T, ctx context.Context, url, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer claude-token")
	req.Header.Set("X-Api-Key", "claude-key")
	req.Header.Set("Cookie", "session=claude")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "claude-code-20250219")
	req.Header.Set("X-Stainless-Lang", "js")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

type event struct {
	name string
	data gjson.Result
}

// events reads an Anthropic event stream, checking each event is well
// formed: named, with JSON data whose type is the name.
func events(t *testing.T, resp *http.Response) []event {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("status %d, Content-Type = %q: %s", resp.StatusCode, ct, readAll(t, resp))
	}
	var out []event
	var name string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "event: "); ok {
			name = v
		}
		if v, ok := strings.CutPrefix(line, "data: "); ok {
			if !gjson.Valid(v) {
				t.Fatalf("event %s: data is not JSON: %s", name, v)
			}
			data := gjson.Parse(v)
			if got := data.Get("type").String(); got != name {
				t.Errorf("event %q carries type %q", name, got)
			}
			out = append(out, event{name, data})
		}
	}
	return out
}

func names(evs []event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = ev.name
	}
	return out
}

// joined concatenates one field over every event of a kind.
func joined(evs []event, name, path string) string {
	var b strings.Builder
	for _, ev := range evs {
		if ev.name == name {
			b.WriteString(ev.data.Get(path).String())
		}
	}
	return b.String()
}

func count(evs []event, name string) int {
	n := 0
	for _, ev := range evs {
		if ev.name == name {
			n++
		}
	}
	return n
}

// checkEnvelope checks the frame every complete Anthropic stream has.
func checkEnvelope(t *testing.T, evs []event, model string) {
	t.Helper()
	if len(evs) < 3 {
		t.Fatalf("events = %v", names(evs))
	}
	first, last := evs[0], evs[len(evs)-1]
	if first.name != "message_start" || last.name != "message_stop" {
		t.Errorf("events = %v", names(evs))
	}
	if got := first.data.Get("message.model").String(); got != model {
		t.Errorf("model = %q, want %q", got, model)
	}
	if n := count(evs, "message_start"); n != 1 {
		t.Errorf("%d message_start events", n)
	}
	if starts, stops := count(evs, "content_block_start"), count(evs, "content_block_stop"); starts != stops {
		t.Errorf("%d blocks started, %d stopped", starts, stops)
	}
}

// checkNoLeak checks nothing of the client's own reached the provider.
func checkNoLeak(t *testing.T, got *captured) {
	t.Helper()
	for name, values := range got.header {
		lower := strings.ToLower(name)
		if lower == "cookie" || strings.HasPrefix(lower, "anthropic-") || strings.HasPrefix(lower, "x-stainless-") {
			t.Errorf("header %s reached the provider", name)
		}
		for _, v := range values {
			if strings.Contains(v, "claude") {
				t.Errorf("header %s = %q reached the provider", name, v)
			}
		}
	}
}

func TestChatStreaming(t *testing.T) {
	var got captured
	tr := translate(t, "chat", record(&got, chatReply))
	body := strings.Replace(messagesRequest, "cameo-helper", "cameo-helper[1m]", 1)
	evs := events(t, ask(t, context.Background(), tr.url+"/v1/messages?beta=true", body))

	if got.path != "/base/chat/completions" || got.query != "" {
		t.Errorf("provider got %s?%s", got.path, got.query)
	}
	if auth := got.header.Get("Authorization"); auth != "Bearer provider-key" {
		t.Errorf("Authorization = %q", auth)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if accept := got.header.Get("Accept"); !strings.Contains(accept, "text/event-stream") {
		t.Errorf("Accept = %q", accept)
	}
	checkNoLeak(t, &got)
	sent := gjson.Parse(got.body)
	for path, want := range map[string]string{
		"model":                        "provider-model",
		"stream":                       "true",
		"messages.0.role":              "system",
		"messages.0.content":           "You are a weather assistant.",
		"messages.1.role":              "user",
		"messages.1.content":           "What is the weather in Paris?",
		"tools.0.type":                 "function",
		"tools.0.function.name":        "get_weather",
		"stream_options.include_usage": "true",
	} {
		if v := sent.Get(path).String(); v != want {
			t.Errorf("sent %s = %q, want %q", path, v, want)
		}
	}

	checkEnvelope(t, evs, "cameo-helper[1m]")
	if text := joined(evs, "content_block_delta", "delta.text"); text != "Let me check." {
		t.Errorf("text = %q", text)
	}
	if args := joined(evs, "content_block_delta", "delta.partial_json"); args != `{"city":"Paris"}` {
		t.Errorf("tool arguments = %q", args)
	}
	var blocks []string
	for _, ev := range evs {
		if ev.name == "content_block_start" {
			blocks = append(blocks, ev.data.Get("content_block.type").String())
			if ev.data.Get("content_block.type").String() == "tool_use" {
				if name, id := ev.data.Get("content_block.name").String(), ev.data.Get("content_block.id").String(); name != "get_weather" || id != "call_abc" {
					t.Errorf("tool_use block = %s", ev.data.Get("content_block").Raw)
				}
			}
		}
	}
	if strings.Join(blocks, ",") != "text,tool_use" {
		t.Errorf("blocks = %v", blocks)
	}
	if stop := joined(evs, "message_delta", "delta.stop_reason"); stop != "tool_use" {
		t.Errorf("stop_reason = %q", stop)
	}
	if out := joined(evs, "message_delta", "usage.output_tokens"); out != "12" {
		t.Errorf("output_tokens = %q", out)
	}
}

func TestChatNonStreaming(t *testing.T) {
	var got captured
	tr := translate(t, "chat", record(&got, chatReply))
	body := strings.Replace(messagesRequest, `"stream": true`, `"stream": false`, 1)
	resp := ask(t, context.Background(), tr.url+"/v1/messages", body)

	if !gjson.Get(got.body, "stream").Bool() {
		t.Errorf("the provider was not asked to stream: %s", got.body)
	}
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || ct != "application/json" {
		t.Fatalf("status %d, Content-Type %q", resp.StatusCode, ct)
	}
	reply := gjson.Parse(readAll(t, resp))
	for path, want := range map[string]string{
		"type":                 "message",
		"role":                 "assistant",
		"model":                "cameo-helper",
		"content.#":            "2",
		"content.0.type":       "text",
		"content.0.text":       "Let me check.",
		"content.1.type":       "tool_use",
		"content.1.name":       "get_weather",
		"content.1.input.city": "Paris",
		"stop_reason":          "tool_use",
		"usage.input_tokens":   "25",
		"usage.output_tokens":  "12",
	} {
		if v := reply.Get(path).String(); v != want {
			t.Errorf("reply %s = %q, want %q", path, v, want)
		}
	}
}

func TestResponsesUpstream(t *testing.T) {
	var got captured
	tr := translate(t, "responses", record(&got, responsesReply))
	evs := events(t, ask(t, context.Background(), tr.url+"/v1/messages", messagesRequest))

	if got.path != "/base/responses" {
		t.Errorf("provider got %s", got.path)
	}
	if auth := got.header.Get("Authorization"); auth != "Bearer provider-key" {
		t.Errorf("Authorization = %q", auth)
	}
	checkNoLeak(t, &got)
	sent := gjson.Parse(got.body)
	for path, want := range map[string]string{
		"model":        "provider-model",
		"stream":       "true",
		"instructions": "You are a weather assistant.",
		"tools.0.type": "function",
		"tools.0.name": "get_weather",
	} {
		if v := sent.Get(path).String(); v != want {
			t.Errorf("sent %s = %q, want %q", path, v, want)
		}
	}
	if !sent.Get("input").IsArray() || sent.Get("messages").Exists() {
		t.Errorf("not a Responses body: %s", got.body)
	}

	checkEnvelope(t, evs, "cameo-helper")
	if text := joined(evs, "content_block_delta", "delta.text"); text != "Hello there" {
		t.Errorf("text = %q", text)
	}
	if stop := joined(evs, "message_delta", "delta.stop_reason"); stop != "end_turn" {
		t.Errorf("stop_reason = %q", stop)
	}
}

func TestGeminiUpstream(t *testing.T) {
	var got captured
	tr := translate(t, "gemini", record(&got, geminiReply))
	evs := events(t, ask(t, context.Background(), tr.url+"/v1/messages", messagesRequest))

	if got.path != "/base/models/provider-model:streamGenerateContent" || got.query != "alt=sse" {
		t.Errorf("provider got %s?%s", got.path, got.query)
	}
	if key := got.header.Get("X-Goog-Api-Key"); key != "provider-key" {
		t.Errorf("X-Goog-Api-Key = %q", key)
	}
	if auth := got.header.Get("Authorization"); auth != "" {
		t.Errorf("Authorization = %q", auth)
	}
	checkNoLeak(t, &got)
	sent := gjson.Parse(got.body)
	for path, want := range map[string]string{
		"contents.0.role":                     "user",
		"contents.0.parts.0.text":             "What is the weather in Paris?",
		"systemInstruction.parts.0.text":      "You are a weather assistant.",
		"tools.0.functionDeclarations.0.name": "get_weather",
		"generationConfig.maxOutputTokens":    "1024",
	} {
		if v := sent.Get(path).String(); v != want {
			t.Errorf("sent %s = %q, want %q", path, v, want)
		}
	}
	// Google reads the model and the streaming from the URL and refuses a
	// body naming them.
	for _, field := range []string{"model", "stream"} {
		if sent.Get(field).Exists() {
			t.Errorf("body carries %s: %s", field, got.body)
		}
	}

	checkEnvelope(t, evs, "cameo-helper")
	if text := joined(evs, "content_block_delta", "delta.text"); text != "Hello there" {
		t.Errorf("text = %q", text)
	}
}

func TestUpstreamErrors(t *testing.T) {
	const openAIError = `{"error":{"message":"the provider said no","type":"some_error","code":"x"}}`
	for _, tc := range []struct {
		status  int
		body    string
		typ     string
		message string
	}{
		{400, openAIError, "invalid_request_error", "the provider said no"},
		{401, openAIError, "authentication_error", "the provider said no"},
		{403, openAIError, "permission_error", "the provider said no"},
		{404, openAIError, "not_found_error", "the provider said no"},
		{413, openAIError, "request_too_large", "the provider said no"},
		{429, openAIError, "rate_limit_error", "the provider said no"},
		{500, openAIError, "api_error", "the provider said no"},
		{502, "  <html>bad gateway</html>\n", "api_error", "<html>bad gateway</html>"},
		{503, `{"error":"model is loading"}`, "overloaded_error", "model is loading"},
		{529, "", "overloaded_error", "529"},
		{422, `{"error":{"code":422,"message":"gemini style","status":"INVALID_ARGUMENT"}}`, "invalid_request_error", "gemini style"},
	} {
		tr := translate(t, "chat", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(tc.status)
			io.WriteString(w, tc.body)
		})
		resp := ask(t, context.Background(), tr.url+"/v1/messages", messagesRequest)
		reply := gjson.Parse(readAll(t, resp))

		if resp.StatusCode != tc.status {
			t.Errorf("%d: status = %d", tc.status, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("%d: Content-Type = %q", tc.status, ct)
		}
		if got := resp.Header.Get("Retry-After"); got != "7" {
			t.Errorf("%d: Retry-After = %q", tc.status, got)
		}
		if reply.Get("type").String() != "error" || reply.Get("error.type").String() != tc.typ {
			t.Errorf("%d: reply = %s", tc.status, reply.Raw)
		}
		if msg := reply.Get("error.message").String(); !strings.Contains(msg, tc.message) {
			t.Errorf("%d: message = %q, want it to carry %q", tc.status, msg, tc.message)
		}
		if raw := strings.TrimSpace(tc.body); raw != "" && !strings.Contains(tr.logs.String(), raw) {
			t.Errorf("%d: raw error body is not in the log: %s", tc.status, tr.logs)
		}
	}
}

func TestErrorInsideStream(t *testing.T) {
	tr := translate(t, "chat", func(w http.ResponseWriter, r *http.Request) {
		sse(w, "data: {\"error\":{\"message\":\"upstream fell over\",\"code\":\"server_error\"}}\n\n"+chatHello)
	})
	evs := events(t, ask(t, context.Background(), tr.url+"/v1/messages", messagesRequest))

	last := evs[len(evs)-1]
	if last.name != "error" || !strings.Contains(last.data.Get("error.message").String(), "upstream fell over") {
		t.Errorf("events = %v, last = %s", names(evs), last.data.Raw)
	}
	if text := joined(evs, "content_block_delta", "delta.text"); text != "" {
		t.Errorf("text after the error: %q", text)
	}
}

func TestStreamCutShort(t *testing.T) {
	tr := translate(t, "chat", func(w http.ResponseWriter, r *http.Request) {
		// one whole chunk, then half of one, then the connection drops
		chunk, _, _ := strings.Cut(chatHello, "\n\n")
		sse(w, chunk+"\n\ndata: {\"id\":")
		panic(http.ErrAbortHandler)
	})
	evs := events(t, ask(t, context.Background(), tr.url+"/v1/messages", messagesRequest))

	if text := joined(evs, "content_block_delta", "delta.text"); text != "Hello" {
		t.Errorf("text = %q", text)
	}
	if last := evs[len(evs)-1]; last.name != "error" {
		t.Errorf("events = %v", names(evs))
	}
	if count(evs, "message_stop") != 0 {
		t.Errorf("a cut stream was finished: %v", names(evs))
	}
}

func TestNotAnEventStream(t *testing.T) {
	tr := translate(t, "chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"Hello"}}]}`)
	})
	for _, stream := range []string{"true", "false"} {
		body := strings.Replace(messagesRequest, `"stream": true`, `"stream": `+stream, 1)
		resp := ask(t, context.Background(), tr.url+"/v1/messages", body)
		reply := gjson.Parse(readAll(t, resp))
		if resp.StatusCode != http.StatusBadGateway || reply.Get("error.type").String() != "api_error" {
			t.Errorf("stream %s: status %d, reply %s", stream, resp.StatusCode, reply.Raw)
		}
	}
}

func TestClientCancelReachesUpstream(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	tr := translate(t, "chat", func(w http.ResponseWriter, r *http.Request) {
		chunk, _, _ := strings.Cut(chatHello, "\n\n")
		sse(w, chunk+"\n\n")
		close(started)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(10 * time.Second):
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := ask(t, ctx, tr.url+"/v1/messages", messagesRequest)
	<-started
	// the reply is flowing before the client gives up
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream request was not cancelled")
	}
}

func TestKeepalive(t *testing.T) {
	defer func(d time.Duration) { keepaliveInterval = d }(keepaliveInterval)
	keepaliveInterval = 20 * time.Millisecond

	tr := translate(t, "chat", func(w http.ResponseWriter, r *http.Request) {
		// silent before the first word, and again in the middle
		sse(w, "")
		time.Sleep(150 * time.Millisecond)
		first, rest, _ := strings.Cut(chatHello, "\n\n")
		sse(w, first+"\n\n")
		time.Sleep(150 * time.Millisecond)
		sse(w, rest)
	})
	evs := events(t, ask(t, context.Background(), tr.url+"/v1/messages", messagesRequest))

	checkEnvelope(t, evs, "cameo-helper")
	if evs[1].name != "ping" {
		t.Errorf("no ping while the upstream had said nothing: %v", names(evs))
	}
	var before, after int
	seenText := false
	for _, ev := range evs {
		switch {
		case ev.name == "content_block_delta":
			seenText = true
		case ev.name == "ping" && seenText:
			after++
		case ev.name == "ping":
			before++
		}
	}
	if before < 2 || after < 2 {
		t.Errorf("%d pings before the text and %d after: %v", before, after, names(evs))
	}
	if text := joined(evs, "content_block_delta", "delta.text"); text != "Hello" {
		t.Errorf("text = %q", text)
	}
}

func TestNoKeepaliveWhileTalking(t *testing.T) {
	tr := translate(t, "chat", record(new(captured), chatReply))
	evs := events(t, ask(t, context.Background(), tr.url+"/v1/messages", messagesRequest))
	if n := count(evs, "ping"); n != 0 {
		t.Errorf("%d pings in a reply that never paused", n)
	}
}

func TestCountTokensIsLocal(t *testing.T) {
	var calls atomic.Int32
	tr := translate(t, "chat", func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	resp := ask(t, context.Background(), tr.url+"/v1/messages/count_tokens?beta=true", messagesRequest)
	reply := gjson.Parse(readAll(t, resp))

	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("status %d, Content-Type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if n := reply.Get("input_tokens").Int(); n < 50 || n > int64(len(messagesRequest)) {
		t.Errorf("input_tokens = %d for a request of %d bytes", n, len(messagesRequest))
	}
	if calls.Load() != 0 {
		t.Error("the provider was called")
	}
}

func TestOtherPathsAreNotFound(t *testing.T) {
	var calls atomic.Int32
	tr := translate(t, "chat", func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	resp := ask(t, context.Background(), tr.url+"/v1/messages/batches", messagesRequest)
	reply := gjson.Parse(readAll(t, resp))

	if resp.StatusCode != http.StatusNotFound || reply.Get("type").String() != "error" || reply.Get("error.type").String() != "not_found_error" {
		t.Errorf("status %d, reply %s", resp.StatusCode, reply.Raw)
	}
	if calls.Load() != 0 {
		t.Error("the provider was called")
	}
}

func TestBadRequestBody(t *testing.T) {
	tr := translate(t, "chat", func(w http.ResponseWriter, r *http.Request) { t.Error("the provider was called") })
	resp := ask(t, context.Background(), tr.url+"/v1/messages", `{"model":"cameo-helper","messages":"nope"}`)
	reply := gjson.Parse(readAll(t, resp))
	if resp.StatusCode != http.StatusBadRequest || reply.Get("error.type").String() != "invalid_request_error" {
		t.Errorf("status %d, reply %s", resp.StatusCode, reply.Raw)
	}
}

// refreshing is a backend whose credential can be renewed a number of times.
type refreshing struct {
	backend
	refreshes atomic.Int32
}

func (b *refreshing) unauthorized(context.Context) bool {
	b.refreshes.Add(1)
	return true
}

func TestUnauthorizedIsRetriedOnce(t *testing.T) {
	for name, tc := range map[string]struct {
		rejections int32 // how many requests the provider answers 401
		status     int
		calls      int32
	}{
		"refresh helps":   {1, http.StatusOK, 2},
		"refresh is vain": {5, http.StatusUnauthorized, 2},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			tr := translate(t, "chat", func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) <= tc.rejections {
					w.WriteHeader(http.StatusUnauthorized)
					io.WriteString(w, `{"error":{"message":"token expired"}}`)
					return
				}
				sse(w, chatHello)
			})
			rt := tr.proxy.routes["cameo-helper"]
			rt.backend = &refreshing{backend: rt.backend}

			resp := ask(t, context.Background(), tr.url+"/v1/messages", messagesRequest)
			body := readAll(t, resp)
			if resp.StatusCode != tc.status || calls.Load() != tc.calls {
				t.Errorf("status %d after %d calls: %s", resp.StatusCode, calls.Load(), body)
			}
			if tc.status == http.StatusOK && !strings.Contains(body, "Hello") {
				t.Errorf("reply = %s", body)
			}
		})
	}
}

func TestKeyBackendDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	tr := translate(t, "chat", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	resp := ask(t, context.Background(), tr.url+"/v1/messages", messagesRequest)
	if resp.StatusCode != http.StatusUnauthorized || calls.Load() != 1 {
		t.Errorf("status %d after %d calls", resp.StatusCode, calls.Load())
	}
}
