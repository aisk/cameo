package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// What a Claude Code subagent sends on its second turn: a system prompt in
// blocks, a tool and Anthropic's web search, one tool call and its result,
// and the knobs a ChatGPT sign-in is refused for.
const reviewRequest = `{
  "model": "cameo-helper",
  "max_tokens": 4096,
  "temperature": 1,
  "top_p": 0.9,
  "stream": true,
  "metadata": {"user_id": "user_abc"},
  "system": [
    {"type": "text", "text": "You are a code reviewer.", "cache_control": {"type": "ephemeral"}},
    {"type": "text", "text": "Never modify anything."}
  ],
  "thinking": {"type": "enabled", "budget_tokens": 8000},
  "tools": [{
    "name": "Read",
    "description": "Read a file.",
    "input_schema": {"type": "object", "properties": {"file_path": {"type": "string"}}, "required": ["file_path"]}
  }, {"type": "web_search_20250305", "name": "web_search"}],
  "tool_choice": {"type": "any"},
  "messages": [
    {"role": "user", "content": "Review main.go"},
    {"role": "assistant", "content": [
      {"type": "text", "text": "Reading it."},
      {"type": "tool_use", "id": "toolu_01", "name": "Read", "input": {"file_path": "main.go"}}
    ]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_01", "content": "package main"}]}
  ]
}`

const firstTurn = `{
  "model": "cameo-helper",
  "max_tokens": 4096,
  "stream": true,
  "system": "You are a code reviewer.",
  "messages": [{"role": "user", "content": "Review main.go"}]
}`

const modelList = `{"models": [
  {"slug": "gpt-5.5", "display_name": "GPT-5.5", "visibility": "list", "context_window": 272000,
   "supported_reasoning_levels": [{"effort": "low"}, {"effort": "medium"}, {"effort": "high"}]},
  {"slug": "gpt-hidden", "display_name": "Hidden", "visibility": "hide"},
  {"slug": "gpt-5.4-mini", "display_name": "GPT-5.4 mini",
   "supported_reasoning_levels": [{"effort": "low"}, {"effort": "medium"}]}
]}`

// chatgpt is a fake api.openai.com behind a proxy with one agent, "helper",
// served by the chatgpt provider with the model gpt-5.5.
type chatgpt struct {
	*translated
	mu    sync.Mutex
	turns []captured
	// reply answers a turn.
	reply func(w http.ResponseWriter, r *http.Request)
}

func newChatGPT(t *testing.T) *chatgpt {
	t.Helper()
	c := &chatgpt{}
	c.reply = func(w http.ResponseWriter, r *http.Request) { sse(w, responsesReply) }
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			t.Errorf("the API was asked for %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		c.mu.Lock()
		c.turns = append(c.turns, captured{path: r.URL.Path, query: r.URL.RawQuery, header: r.Header, body: string(body)})
		c.mu.Unlock()
		c.reply(w, r)
	})
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Anthropic upstream was called: %s %s", r.Method, r.URL)
	}))
	t.Cleanup(anthropic.Close)

	cfg := &Config{Upstream: anthropic.URL, Agents: map[string]*Agent{"helper": {Provider: chatgptProvider, Model: "gpt-5.5"}}}
	logs := new(lockedBuffer)
	p, err := newProxy(cfg, "secret", log.New(logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	c.translated = &translated{url: srv.URL + "/secret", proxy: p, logs: logs}
	return c
}

func (c *chatgpt) turn(n int) captured {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turns[n]
}

func (c *chatgpt) asked() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.turns)
}

func TestChatGPTRequest(t *testing.T) {
	tempAuth(t)
	saveSignIn(t, testSignIn(time1h()))
	c := newChatGPT(t)

	evs := events(t, ask(t, context.Background(), c.url+"/v1/messages", reviewRequest))
	checkEnvelope(t, evs, "cameo-helper")
	if text := joined(evs, "content_block_delta", "delta.text"); text != "Hello there" {
		t.Errorf("text = %q", text)
	}

	got := c.turn(0)
	if got.path != "/v1/responses" || got.query != "" {
		t.Errorf("asked for %s?%s", got.path, got.query)
	}
	checkNoLeak(t, &got)
	// The token says who is asking, and nothing else does.
	var sent []string
	for name := range got.header {
		sent = append(sent, name)
	}
	sort.Strings(sent)
	if got := strings.Join(sent, " "); got != "Accept Accept-Encoding Authorization Content-Length Content-Type User-Agent" {
		t.Errorf("headers = %s", got)
	}
	for name, want := range map[string]string{
		"Authorization": "Bearer access-0",
		"Accept":        "text/event-stream",
		"Content-Type":  "application/json",
	} {
		if v := got.header.Get(name); v != want {
			t.Errorf("header %s = %q, want %q", name, v, want)
		}
	}
	if ua := got.header.Get("User-Agent"); strings.Contains(strings.ToLower(ua), "codex") {
		t.Errorf("User-Agent = %q", ua)
	}

	body := gjson.Parse(got.body)
	for _, field := range append([]string{"max_tokens", "prompt_cache_key", "text", "service_tier"}, chatgptDropped...) {
		if body.Get(field).Exists() {
			t.Errorf("%s was sent: %s", field, body.Get(field).Raw)
		}
	}
	for path, want := range map[string]string{
		"model":  "gpt-5.5",
		"stream": "true",
		"store":  "false",
		// The agent's own instructions, where instructions go.
		"instructions":           "You are a code reviewer.Never modify anything.",
		"tool_choice":            "required",
		"reasoning.effort":       "medium",
		"input.#":                "4",
		"input.0.role":           "user",
		"input.0.content.0.text": "Review main.go",
		"input.1.role":           "assistant",
		"input.1.content.0.text": "Reading it.",
		"input.2.type":           "function_call",
		"input.2.name":           "Read",
		"input.3.type":           "function_call_output",
		"input.3.output":         "package main",
		// Claude Code's own tool, and OpenAI's web search for Anthropic's.
		"tools.#":      "2",
		"tools.0.type": "function",
		"tools.0.name": "Read",
		"tools.1.type": "web_search",
	} {
		if v := body.Get(path).String(); v != want {
			t.Errorf("%s = %q, want %q", path, v, want)
		}
	}
	if call, output := body.Get("input.2.call_id").String(), body.Get("input.3.call_id").String(); call == "" || call != output {
		t.Errorf("call ids %q and %q", call, output)
	}
	if strings.Contains(got.body, "Codex") || strings.Contains(got.body, `"system"`) {
		t.Errorf("body = %s", got.body)
	}
}

func TestChatGPTNonStreaming(t *testing.T) {
	tempAuth(t)
	saveSignIn(t, testSignIn(time1h()))
	c := newChatGPT(t)
	resp := ask(t, context.Background(), c.url+"/v1/messages", strings.Replace(firstTurn, `"stream": true`, `"stream": false`, 1))
	reply := gjson.Parse(readAll(t, resp))
	if resp.StatusCode != http.StatusOK || reply.Get("content.0.text").String() != "Hello there" || reply.Get("usage.input_tokens").Int() != 7 {
		t.Errorf("status %d: %s", resp.StatusCode, reply.Raw)
	}
	// A ChatGPT sign-in is answered with a stream only.
	if !gjson.Get(c.turn(0).body, "stream").Bool() {
		t.Error("the API was not asked to stream")
	}
}

// A token near its end is renewed before the request is made.
func TestChatGPTRenewsFirst(t *testing.T) {
	newFakeAuth(t)
	tempAuth(t)
	saveSignIn(t, testSignIn(time.Now().Add(time.Minute)))
	c := newChatGPT(t)
	readAll(t, ask(t, context.Background(), c.url+"/v1/messages", firstTurn))
	if got := c.turn(0).header.Get("Authorization"); got != "Bearer access-1" || c.asked() != 1 {
		t.Errorf("Authorization = %q after %d requests", got, c.asked())
	}
}

// A 401 from OpenAI renews the tokens and the request goes once more.
func TestChatGPTUnauthorized(t *testing.T) {
	auth := newFakeAuth(t)
	path := tempAuth(t)
	saveSignIn(t, testSignIn(time1h()))
	c := newChatGPT(t)
	c.reply = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-1" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"token expired","code":"token_expired"}}`)
			return
		}
		sse(w, responsesReply)
	}

	resp := ask(t, context.Background(), c.url+"/v1/messages", firstTurn)
	if body := readAll(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(body, "Hello") {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if refreshes, _, _ := auth.counts(); c.asked() != 2 || refreshes != 1 {
		t.Errorf("%d requests, %d renewals", c.asked(), refreshes)
	}
	if saved := readAuthFile(t, path)[chatgptProvider]; saved["access_token"] != "access-1" {
		t.Errorf("saved %v", saved)
	}
}

// When renewing does not help, the request is not sent a third time, and
// the client is not told its own login is bad.
func TestChatGPTStillUnauthorized(t *testing.T) {
	for name, refuse := range map[string]string{"renewed in vain": "", "sign-in gone": "invalid_grant"} {
		t.Run(name, func(t *testing.T) {
			auth := newFakeAuth(t)
			auth.refuse = refuse
			tempAuth(t)
			saveSignIn(t, testSignIn(time1h()))
			c := newChatGPT(t)
			c.reply = func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, `{"detail":"Could not validate your credentials"}`)
			}

			resp := ask(t, context.Background(), c.url+"/v1/messages", firstTurn)
			reply := gjson.Parse(readAll(t, resp))
			if resp.StatusCode != signInStatus || reply.Get("error.type").String() != "invalid_request_error" {
				t.Errorf("status %d: %s", resp.StatusCode, reply.Raw)
			}
			msg := reply.Get("error.message").String()
			if !strings.Contains(msg, "Could not validate your credentials") || !strings.Contains(msg, "cameo provider login chatgpt") {
				t.Errorf("message = %q", msg)
			}
			want := 2
			if refuse != "" {
				want = 1
			}
			if c.asked() != want {
				t.Errorf("%d requests, want %d", c.asked(), want)
			}
		})
	}
}

// A sign-in that cannot be renewed is the client's to hear of, in a way
// Claude Code does not take for its own login.
func TestChatGPTSignInProblems(t *testing.T) {
	expired := func() *signIn { return testSignIn(time.Now().Add(-time.Minute)) }
	for name, tc := range map[string]struct {
		in      *signIn
		setup   func(*fakeAuth)
		status  int
		typ     string
		message string
	}{
		"signed out":  {nil, func(*fakeAuth) {}, http.StatusBadRequest, "invalid_request_error", "cameo provider login chatgpt"},
		"gone":        {expired(), func(f *fakeAuth) { f.refuse = "refresh_token_expired" }, http.StatusBadRequest, "invalid_request_error", "cameo provider login chatgpt"},
		"client gone": {expired(), func(f *fakeAuth) { f.refuse = "invalid_client" }, http.StatusBadRequest, "invalid_request_error", "no longer knows the client"},
		"hiccup":      {expired(), func(f *fakeAuth) { f.refuseStatus = http.StatusBadGateway }, http.StatusBadGateway, "api_error", "ChatGPT token refresh"},
	} {
		t.Run(name, func(t *testing.T) {
			tc.setup(newFakeAuth(t))
			tempAuth(t)
			if tc.in != nil {
				saveSignIn(t, tc.in)
			}
			c := newChatGPT(t)

			resp := ask(t, context.Background(), c.url+"/v1/messages", firstTurn)
			reply := gjson.Parse(readAll(t, resp))
			if resp.StatusCode != tc.status || reply.Get("type").String() != "error" || reply.Get("error.type").String() != tc.typ {
				t.Errorf("status %d: %s", resp.StatusCode, reply.Raw)
			}
			if msg := reply.Get("error.message").String(); !strings.Contains(msg, tc.message) {
				t.Errorf("message = %q", msg)
			}
			if c.asked() != 0 {
				t.Errorf("the API was asked %d times", c.asked())
			}
		})
	}
}

// OpenAI's refusals of a ChatGPT sign-in reach the client with what can be
// done about them.
func TestChatGPTErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status  int
		body    string
		typ     string
		message []string
	}{
		"usage limit":   {429, `{"error":{"code":"subscription_sharing_usage_limit_exceeded","message":"Usage limit exceeded"}}`, "rate_limit_error", []string{"chatgpt: Usage limit exceeded (", "shared with ChatGPT", "https://chatgpt.com/settings/usage"}},
		"not eligible":  {403, `{"error":{"code":"subscription_sharing_user_not_eligible","message":"Not eligible"}}`, "permission_error", []string{"Not eligible (", "plan isn't eligible"}},
		"capability":    {400, `{"error":{"code":"subscription_sharing_unsupported_capability","message":"Unsupported","param":"tools[1]"}}`, "invalid_request_error", []string{"Unsupported (a ChatGPT sign-in can't use tools[1]"}},
		"detail object": {403, `{"detail":{"code":"subscription_sharing_invalid_user","message":"Invalid user"}}`, "permission_error", []string{"chatgpt: Invalid user (", "cameo provider login chatgpt"}},
		"detail text":   {403, `{"detail":"Blocked: chatpass_v2_region."}`, "permission_error", []string{"chatgpt: Blocked: chatpass_v2_region. (ChatGPT refused the account (chatpass_v2_region))"}},
		"plain detail":  {400, `{"detail":"Unsupported parameter: truncation"}`, "invalid_request_error", []string{"chatgpt: Unsupported parameter: truncation"}},
		"plain error":   {404, `{"error":{"message":"Model not found","code":"model_not_found"}}`, "not_found_error", []string{"chatgpt: Model not found"}},
	} {
		t.Run(name, func(t *testing.T) {
			tempAuth(t)
			saveSignIn(t, testSignIn(time1h()))
			c := newChatGPT(t)
			c.reply = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "9")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}
			resp := ask(t, context.Background(), c.url+"/v1/messages", firstTurn)
			reply := gjson.Parse(readAll(t, resp))
			if resp.StatusCode != tc.status || reply.Get("error.type").String() != tc.typ || resp.Header.Get("Retry-After") != "9" {
				t.Errorf("status %d: %s", resp.StatusCode, reply.Raw)
			}
			msg := reply.Get("error.message").String()
			for _, want := range tc.message {
				if !strings.Contains(msg, want) {
					t.Errorf("message = %q, want it to carry %q", msg, want)
				}
			}
			if strings.HasPrefix(name, "plain") && strings.Contains(msg, "(") {
				t.Errorf("message = %q, with nothing to add", msg)
			}
			if c.asked() != 1 {
				t.Errorf("%d requests", c.asked())
			}
		})
	}
}

// Another provider's refusal is not read as a ChatGPT sign-in's.
func TestExplanationsAreChatGPTsOnly(t *testing.T) {
	tr := translate(t, "responses", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"code":"subscription_sharing_usage_limit_exceeded","message":"limit"}}`)
	})
	resp := ask(t, context.Background(), tr.url+"/v1/messages", messagesRequest)
	if msg := gjson.Get(readAll(t, resp), "error.message").String(); msg != "third: limit" {
		t.Errorf("message = %q", msg)
	}
}

func TestChatGPTErrorInsideStream(t *testing.T) {
	tempAuth(t)
	saveSignIn(t, testSignIn(time1h()))
	c := newChatGPT(t)
	c.reply = func(w http.ResponseWriter, r *http.Request) {
		sse(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n"+
			"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_1\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"it fell over\"}}}\n\n")
	}
	evs := events(t, ask(t, context.Background(), c.url+"/v1/messages", firstTurn))
	last := evs[len(evs)-1]
	if last.name != "error" || !strings.Contains(last.data.Get("error.message").String(), "it fell over") {
		t.Errorf("events = %v, last = %s", names(evs), last.data.Raw)
	}
}

// Requests turned away together renew the tokens once between them.
func TestChatGPTUnauthorizedTogether(t *testing.T) {
	auth := newFakeAuth(t)
	auth.delay = 20 * time.Millisecond
	tempAuth(t)
	saveSignIn(t, testSignIn(time1h()))
	c := newChatGPT(t)
	c.reply = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sse(w, responsesReply)
	}
	var ok atomic.Int32
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			resp := ask(t, context.Background(), c.url+"/v1/messages", firstTurn)
			if readAll(t, resp); resp.StatusCode == http.StatusOK {
				ok.Add(1)
			}
		})
	}
	wg.Wait()
	if refreshes, _, asked := auth.counts(); ok.Load() != 6 || refreshes != 1 || asked != 1 {
		t.Errorf("%d of 6 answered, %d renewals of %d asked for", ok.Load(), refreshes, asked)
	}
}
