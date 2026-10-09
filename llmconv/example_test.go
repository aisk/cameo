package llmconv_test

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aisk/cameo/llmconv"
	"github.com/tidwall/gjson"
)

// An Anthropic Messages request: one tool, and a turn in which the model
// called it and was given the result.
const anthropicRequest = `{
  "model": "claude-sonnet-4-5",
  "max_tokens": 1024,
  "system": "You are a weather assistant.",
  "tools": [{
    "name": "get_weather",
    "description": "Look up the weather in a city.",
    "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}
  }],
  "messages": [
    {"role": "user", "content": "What is the weather in Paris?"},
    {"role": "assistant", "content": [
      {"type": "text", "text": "Let me check."},
      {"type": "tool_use", "id": "toolu_01", "name": "get_weather", "input": {"city": "Paris"}}
    ]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "toolu_01", "content": "18C and sunny"}
    ]}
  ]
}`

// An OpenAI Chat Completions stream: some text, then one tool call whose
// arguments arrive in two pieces, then the usage.
const chatStream = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Let me "},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"check."},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":25,"completion_tokens":12,"total_tokens":37}}

data: [DONE]

`

func TestAnthropicRequestToChat(t *testing.T) {
	out, err := llmconv.ConvertRequest(llmconv.Anthropic, llmconv.Chat, []byte(anthropicRequest), "gpt-4o", llmconv.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.ValidBytes(out) {
		t.Fatalf("not JSON: %s", out)
	}
	got := gjson.ParseBytes(out)
	for path, want := range map[string]string{
		"model":                                  "gpt-4o",
		"max_tokens":                             "1024",
		"messages.#":                             "4",
		"messages.0.role":                        "system",
		"messages.0.content":                     "You are a weather assistant.",
		"messages.1.role":                        "user",
		"messages.1.content":                     "What is the weather in Paris?",
		"messages.2.role":                        "assistant",
		"messages.2.content":                     "Let me check.",
		"messages.2.tool_calls.#":                "1",
		"messages.2.tool_calls.0.id":             "toolu_01",
		"messages.2.tool_calls.0.type":           "function",
		"messages.2.tool_calls.0.function.name":  "get_weather",
		"messages.3.role":                        "tool",
		"messages.3.tool_call_id":                "toolu_01",
		"messages.3.content":                     "18C and sunny",
		"tools.#":                                "1",
		"tools.0.type":                           "function",
		"tools.0.function.name":                  "get_weather",
		"tools.0.function.description":           "Look up the weather in a city.",
		"tools.0.function.parameters.type":       "object",
		"tools.0.function.parameters.required.0": "city",
	} {
		if s := got.Get(path).String(); s != want {
			t.Errorf("%s = %q, want %q", path, s, want)
		}
	}
	// Chat carries a call's arguments as a JSON string
	args := got.Get("messages.2.tool_calls.0.function.arguments")
	if args.Type != gjson.String || gjson.Get(args.String(), "city").String() != "Paris" {
		t.Errorf("arguments = %s", args.Raw)
	}
}

func TestAnthropicRequestToResponses(t *testing.T) {
	out, err := llmconv.ConvertRequest(llmconv.Anthropic, llmconv.Responses, []byte(anthropicRequest), "gpt-4o", llmconv.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.ValidBytes(out) {
		t.Fatalf("not JSON: %s", out)
	}
	got := gjson.ParseBytes(out)
	for path, want := range map[string]string{
		"model":                       "gpt-4o",
		"instructions":                "You are a weather assistant.",
		"max_output_tokens":           "1024",
		"input.#":                     "4",
		"input.0.type":                "message",
		"input.0.role":                "user",
		"input.0.content.0.type":      "input_text",
		"input.0.content.0.text":      "What is the weather in Paris?",
		"input.1.role":                "assistant",
		"input.1.content.0.type":      "output_text",
		"input.1.content.0.text":      "Let me check.",
		"input.2.type":                "function_call",
		"input.2.call_id":             "toolu_01",
		"input.2.name":                "get_weather",
		"input.3.type":                "function_call_output",
		"input.3.call_id":             "toolu_01",
		"input.3.output":              "18C and sunny",
		"tools.#":                     "1",
		"tools.0.type":                "function",
		"tools.0.name":                "get_weather",
		"tools.0.parameters.type":     "object",
		"tools.0.parameters.required": `["city"]`,
	} {
		if s := got.Get(path).String(); s != want {
			t.Errorf("%s = %q, want %q", path, s, want)
		}
	}
	if args := got.Get("input.2.arguments"); gjson.Get(args.String(), "city").String() != "Paris" {
		t.Errorf("arguments = %s", args.Raw)
	}
}

// A model left empty keeps the one the original request named, and the
// options shape the body for the upstream it goes to.
func TestBuildOptions(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":64,"temperature":0.2,"messages":[{"role":"user","content":"hi"}]}`)
	plain, err := llmconv.ConvertRequest(llmconv.Anthropic, llmconv.Chat, body, "", llmconv.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(plain, "model").String(); got != "claude-sonnet-4-5" {
		t.Errorf("model = %q", got)
	}
	if got := gjson.GetBytes(plain, "temperature").Float(); got != 0.2 {
		t.Errorf("temperature = %v", got)
	}
	if !gjson.GetBytes(plain, "max_tokens").Exists() {
		t.Errorf("no max_tokens: %s", plain)
	}
	shaped, err := llmconv.ConvertRequest(llmconv.Anthropic, llmconv.Chat, body, "o3", llmconv.BuildOptions{Host: "api.openai.com", RejectTemperature: true})
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(shaped, "temperature").Exists() {
		t.Errorf("temperature kept: %s", shaped)
	}
	if got := gjson.GetBytes(shaped, "max_completion_tokens").Int(); got != 64 || gjson.GetBytes(shaped, "max_tokens").Exists() {
		t.Errorf("OpenAI's own API takes max_completion_tokens: %s", shaped)
	}
	if _, err := llmconv.ConvertRequest("nope", llmconv.Chat, body, "", llmconv.BuildOptions{}); err == nil {
		t.Error("an unknown protocol was accepted")
	}
}

// anthropicEvents are the events of an Anthropic stream, each as its name
// and its data.
func anthropicEvents(t *testing.T, body string) (names []string, data []gjson.Result) {
	t.Helper()
	err := llmconv.ReadSSE(strings.NewReader(body), func(event, d string) error {
		if !gjson.Valid(d) {
			t.Errorf("event %s is not JSON: %s", event, d)
		}
		if typ := gjson.Get(d, "type").String(); typ != event {
			t.Errorf("event %s carries type %q", event, typ)
		}
		names = append(names, event)
		data = append(data, gjson.Parse(d))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return names, data
}

func TestChatStreamToAnthropic(t *testing.T) {
	req, err := llmconv.Parse(llmconv.Anthropic, []byte(anthropicRequest))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	if err := llmconv.ConvertStream(llmconv.Chat, llmconv.Anthropic, strings.NewReader(chatStream), rec, req); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	names, data := anthropicEvents(t, rec.Body.String())
	if len(names) < 2 || names[0] != "message_start" || names[len(names)-1] != "message_stop" {
		t.Fatalf("events = %v", names)
	}
	if role := data[0].Get("message.role").String(); role != "assistant" {
		t.Errorf("message_start role = %q", role)
	}

	// what the blocks said, by index
	kind := map[int64]string{}
	text := map[int64]string{}
	var open, closed int
	var call gjson.Result
	var stop gjson.Result
	for i, d := range data {
		idx := d.Get("index").Int()
		switch names[i] {
		case "content_block_start":
			open++
			kind[idx] = d.Get("content_block.type").String()
			if kind[idx] == "tool_use" {
				call = d.Get("content_block")
			}
		case "content_block_delta":
			text[idx] += d.Get("delta.text").String() + d.Get("delta.partial_json").String()
		case "content_block_stop":
			closed++
		case "message_delta":
			stop = d
		}
	}
	if open != 2 || closed != 2 {
		t.Fatalf("%d blocks opened, %d closed: %v", open, closed, names)
	}
	if kind[0] != "text" || text[0] != "Let me check." {
		t.Errorf("block 0 is %s %q", kind[0], text[0])
	}
	if kind[1] != "tool_use" || call.Get("id").String() != "call_abc" || call.Get("name").String() != "get_weather" {
		t.Errorf("block 1 is %s %s", kind[1], call.Raw)
	}
	if got := gjson.Get(text[1], "city").String(); got != "Paris" {
		t.Errorf("the call's arguments came to %q", text[1])
	}
	if got := stop.Get("delta.stop_reason").String(); got != "tool_use" {
		t.Errorf("stop_reason = %q", got)
	}
	if in, out := stop.Get("usage.input_tokens").Int(), stop.Get("usage.output_tokens").Int(); in != 25 || out != 12 {
		t.Errorf("usage = %d in, %d out", in, out)
	}
}

// The same stream gathered into one reply, for a client that did not ask
// for a stream.
func TestChatStreamToAnthropicResponse(t *testing.T) {
	out, err := llmconv.ConvertResponse(llmconv.Chat, llmconv.Anthropic, strings.NewReader(chatStream), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := gjson.ParseBytes(out)
	for path, want := range map[string]string{
		"type":                "message",
		"role":                "assistant",
		"stop_reason":         "tool_use",
		"content.#":           "2",
		"content.0.type":      "text",
		"content.0.text":      "Let me check.",
		"content.1.type":      "tool_use",
		"content.1.id":        "call_abc",
		"content.1.name":      "get_weather",
		"content.1.input":     `{"city":"Paris"}`,
		"usage.input_tokens":  "25",
		"usage.output_tokens": "12",
	} {
		if s := got.Get(path).String(); s != want {
			t.Errorf("%s = %q, want %q", path, s, want)
		}
	}
}

// The pieces ConvertStream is made of, used apart: a decoder's events go
// to a collector, and what it gathered is rendered in another protocol.
func TestDecodeCollectRender(t *testing.T) {
	dec := llmconv.NewDecoder(llmconv.Chat)
	var col llmconv.Collector
	var kinds []llmconv.EventKind
	err := llmconv.ReadSSE(strings.NewReader(chatStream), func(_, data string) error {
		return dec.Decode(data, func(ev llmconv.Event) {
			kinds = append(kinds, ev.Kind)
			col.Add(ev)
		})
	})
	if err != nil || col.Err() != nil {
		t.Fatal(err, col.Err())
	}
	if len(kinds) == 0 || kinds[0] != llmconv.KStart {
		t.Fatalf("events = %v", kinds)
	}
	res := col.Result()
	if res.Stop != "tool" || len(res.Parts) != 2 {
		t.Fatalf("result = %+v", res)
	}
	if p := res.Parts[0]; p.Kind != llmconv.Text || p.Text != "Let me check." {
		t.Errorf("part 0 = %+v", p)
	}
	if p := res.Parts[1]; p.Kind != llmconv.ToolCall || p.Name != "get_weather" || gjson.GetBytes(p.Args, "city").String() != "Paris" {
		t.Errorf("part 1 = %+v", p)
	}
	out := gjson.ParseBytes(llmconv.Render(llmconv.Responses, res, nil))
	if got := out.Get(`output.#(type=="function_call").call_id`).String(); got != "call_abc" {
		t.Errorf("no function_call for call_abc: %s", out.Raw)
	}
	if got := out.Get(`output.#(type=="message").content.0.text`).String(); got != "Let me check." {
		t.Errorf("no message text: %s", out.Raw)
	}
}

// A failure the upstream reports inside its stream reaches the client as
// its own protocol's error event and the caller as an UpstreamError.
func TestUpstreamErrorInStream(t *testing.T) {
	const failed = "data: {\"error\":{\"message\":\"overloaded\",\"type\":\"server_error\"}}\n\n"
	rec := httptest.NewRecorder()
	err := llmconv.ConvertStream(llmconv.Chat, llmconv.Anthropic, strings.NewReader(failed), rec, nil)
	var ue *llmconv.UpstreamError
	if !errors.As(err, &ue) || ue.Message != "overloaded" {
		t.Fatalf("err = %v", err)
	}
	names, data := anthropicEvents(t, rec.Body.String())
	last := len(names) - 1
	if last < 0 || names[last] != "error" || data[last].Get("error.message").String() != "overloaded" {
		t.Errorf("the client was sent %v", names)
	}
	if _, err := llmconv.ConvertResponse(llmconv.Chat, llmconv.Anthropic, strings.NewReader(failed), nil); !errors.As(err, &ue) {
		t.Errorf("err = %v", err)
	}
}
