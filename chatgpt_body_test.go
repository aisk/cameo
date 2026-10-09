package main

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func rewritten(body string) gjson.Result {
	return gjson.ParseBytes(chatgptBody([]byte(body)))
}

func TestChatGPTBodyDrops(t *testing.T) {
	fields := map[string]string{
		"background": "true", "conversation": `"conv_1"`, "max_output_tokens": "1", "max_tool_calls": "2",
		"metadata": `{"a":"b"}`, "moderation": `"auto"`, "multi_agent": "true", "prompt": `{"id":"p"}`,
		"prompt_cache_retention": `"24h"`, "safety_identifier": `"s"`, "temperature": "0.5", "top_logprobs": "1",
		"top_p": "0.9", "truncation": `"auto"`, "user": `"u"`, "previous_response_id": `"resp_1"`,
	}
	if len(fields) != len(chatgptDropped) {
		t.Fatalf("%d fields tried, %d dropped", len(fields), len(chatgptDropped))
	}
	body := `{"model":"m","input":[],"stream":false,"store":true`
	for k, v := range fields {
		body += `,"` + k + `":` + v
	}
	got := rewritten(body + "}")
	for k := range fields {
		if got.Get(k).Exists() {
			t.Errorf("%s was kept", k)
		}
	}
	if got.Get("model").String() != "m" || !got.Get("stream").Bool() || got.Get("store").Bool() || !got.Get("store").Exists() {
		t.Errorf("body = %s", got.Raw)
	}
	if n := len(got.Map()); n != 4 {
		t.Errorf("%d fields: %s", n, got.Raw)
	}
}

// What the agent asked for and OpenAI takes goes on as it was.
func TestChatGPTBodyKeeps(t *testing.T) {
	got := rewritten(`{"model":"m","instructions":"You review code.","service_tier":"priority",
		"prompt_cache_key":"k","parallel_tool_calls":false,"tool_choice":"required",
		"reasoning":{"effort":"high","summary":"auto"},"include":["web_search_call.action.sources"],
		"input":[{"type":"message","role":"user","content":"hi"},
			{"type":"function_call","call_id":"` + strings.Repeat("x", 70) + `","name":"f","arguments":"{}"}],
		"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"n":{"const":12345678901234567890}}}}]}`)
	for path, want := range map[string]string{
		"instructions": "You review code.", "service_tier": "priority", "prompt_cache_key": "k",
		"parallel_tool_calls": "false", "tool_choice": "required", "reasoning.effort": "high",
		"include.0": "web_search_call.action.sources", "input.#": "2", "tools.0.name": "f",
		"input.1.call_id": strings.Repeat("x", 70),
		// a number too long for a float comes through as it was written
		"tools.0.parameters.properties.n.const": "12345678901234567890",
	} {
		if v := got.Get(path).String(); v != want {
			t.Errorf("%s = %q, want %q", path, v, want)
		}
	}
	for _, added := range []string{"text", "prompt_cache_key_derived", "session_id"} {
		if got.Get(added).Exists() {
			t.Errorf("%s was added", added)
		}
	}
	if got := string(chatgptBody([]byte("not json"))); got != "not json" {
		t.Errorf("body = %s", got)
	}
}

func TestChatGPTBodySystemRole(t *testing.T) {
	got := rewritten(`{"input":[
		{"type":"message","role":"system","content":"be kind"},
		{"role":"system","content":"and brief"},
		{"type":"message","role":"user","content":"hi"},
		{"type":"message","role":"assistant","content":"hello"},
		"a stray string"
	]}`).Get("input")
	for path, want := range map[string]string{
		"#": "5", "0.role": "developer", "0.content": "be kind", "1.role": "developer",
		"2.role": "user", "3.role": "assistant", "4": "a stray string",
	} {
		if v := got.Get(path).String(); v != want {
			t.Errorf("%s = %q, want %q", path, v, want)
		}
	}
}

// The tools OpenAI would run itself go, but for web search, and with the
// last tool the tool choice.
func TestChatGPTBodyTools(t *testing.T) {
	hosted := ""
	for _, typ := range chatgptHostedTools {
		hosted += `,{"type":"` + typ + `"}`
	}
	if len(chatgptHostedTools) != 8 {
		t.Fatalf("hosted tools = %v", chatgptHostedTools)
	}
	got := rewritten(`{"tool_choice":"auto","tools":[{"type":"function","name":"shell"}` + hosted + `,{"type":"web_search"}]}`)
	if got.Get("tools.#").Int() != 2 || got.Get("tools.0.name").String() != "shell" || got.Get("tools.1.type").String() != "web_search" ||
		got.Get("tool_choice").String() != "auto" {
		t.Errorf("body = %s", got.Raw)
	}
	got = rewritten(`{"tool_choice":"required","tools":[` + hosted[1:] + `]}`)
	if got.Get("tools").Exists() || got.Get("tool_choice").Exists() {
		t.Errorf("body = %s", got.Raw)
	}
	// No tools to begin with: the choice is the caller's to have made.
	if got := rewritten(`{"tool_choice":"none"}`); got.Get("tool_choice").String() != "none" {
		t.Errorf("body = %s", got.Raw)
	}
}

func TestChatGPTExplain(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"usage limit":     {429, `{"error":{"code":"subscription_sharing_usage_limit_exceeded","message":"limit"}}`, "shared with ChatGPT and the other apps it is used in, is reached, see https://chatgpt.com/settings/usage"},
		"usage unknown":   {503, `{"error":{"code":"subscription_sharing_usage_unavailable"}}`, "try again in a moment"},
		"user unknown":    {503, `{"detail":{"code":"subscription_sharing_user_unavailable","message":"x"}}`, "try again in a moment"},
		"not eligible":    {403, `{"error":{"code":"subscription_sharing_user_not_eligible"}}`, "plan isn't eligible"},
		"capability":      {400, `{"error":{"code":"subscription_sharing_unsupported_capability","param":"tools[2]"}}`, "can't use tools[2] through OpenAI's API"},
		"capability bare": {400, `{"detail":{"code":"subscription_sharing_unsupported_capability"}}`, "can't use part of this request"},
		"detail param":    {400, `{"detail":{"code":"subscription_sharing_unsupported_capability","param":"background"}}`, "can't use background"},
		"route":           {404, `{"detail":"Route refused: subscription_sharing_route_not_supported."}`, "can't use this endpoint"},
		"invalid user":    {403, `{"error":{"code":"subscription_sharing_invalid_user"}}`, "cameo provider login chatgpt"},
		"chatpass":        {403, `{"detail":"chatpass_v2_blocked_region"}`, "ChatGPT refused the account (chatpass_v2_blocked_region)"},
		"chatpass code":   {403, `{"error":{"code":"chatpass_v2_suspended"}}`, "(chatpass_v2_suspended)"},
		"a 401":           {401, `{"error":{"code":"invalid_api_key","message":"bad"}}`, "cameo provider login chatgpt"},
		"something else":  {400, `{"error":{"code":"invalid_request_error","message":"bad"}}`, ""},
		"only a type":     {429, `{"error":{"type":"rate_limit_exceeded","message":"slow"}}`, ""},
		"not json":        {502, `<html>`, ""},
	} {
		got := chatgptExplain(tc.status, []byte(tc.body))
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
