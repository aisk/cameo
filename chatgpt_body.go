package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
)

// chatgptDropped are the Responses fields Sign in with ChatGPT refuses.
// previous_response_id too: nothing is stored, so the whole conversation
// is sent each time.
var chatgptDropped = []string{"background", "conversation", "max_output_tokens", "max_tool_calls", "metadata",
	"moderation", "multi_agent", "prompt", "prompt_cache_retention", "safety_identifier", "temperature",
	"top_logprobs", "top_p", "truncation", "user", "previous_response_id"}

// chatgptHostedTools are the tools OpenAI runs itself that it will not run
// for a ChatGPT token: a request offering one is refused whole. Web search
// is not among them.
var chatgptHostedTools = []string{"image_generation", "file_search", "code_interpreter", "computer_use",
	"computer_use_preview", "computer", "mcp", "tool_search"}

// chatgptBody makes a Responses request one Sign in with ChatGPT takes:
// stored nowhere and streamed, without the fields and tools it refuses,
// and a system message as the developer one it takes in its place. The
// instructions stay the agent's own.
func chatgptBody(body []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if dec.Decode(&m) != nil || m == nil {
		return body
	}
	for _, k := range chatgptDropped {
		delete(m, k)
	}
	m["store"], m["stream"] = false, true
	input, _ := m["input"].([]any)
	for _, it := range input {
		if item, ok := it.(map[string]any); ok && item["role"] == "system" {
			item["role"] = "developer"
		}
	}
	if tools, ok := m["tools"].([]any); ok {
		tools = slices.DeleteFunc(tools, func(t any) bool {
			tool, _ := t.(map[string]any)
			typ, _ := tool["type"].(string)
			return slices.Contains(chatgptHostedTools, typ)
		})
		if len(tools) == 0 {
			delete(m, "tools")
			delete(m, "tool_choice")
		} else {
			m["tools"] = tools
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// chatgptErrorCode is the code a refusal of the API names: error.code, or
// a detail's, which is an object or a text with the code somewhere in it.
func chatgptErrorCode(body []byte) (code, param string) {
	var e struct {
		Error struct {
			Code  string `json:"code"`
			Type  string `json:"type"`
			Param string `json:"param"`
		} `json:"error"`
		Detail any `json:"detail"`
	}
	if json.Unmarshal(body, &e) != nil {
		return "", ""
	}
	if e.Error.Code != "" {
		return e.Error.Code, e.Error.Param
	}
	switch d := e.Detail.(type) {
	case map[string]any:
		code, _ = d["code"].(string)
		param, _ = d["param"].(string)
		return code, param
	case string:
		for _, prefix := range []string{"subscription_sharing_", "chatpass_v2_"} {
			if i := strings.Index(d, prefix); i >= 0 {
				rest := d[i:]
				end := strings.IndexFunc(rest, func(r rune) bool { return !(r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') })
				if end < 0 {
					end = len(rest)
				}
				return rest[:end], ""
			}
		}
	}
	return e.Error.Type, ""
}

// chatgptExplain says what the user can do about a refusal of OpenAI's,
// or nothing when it has nothing to add.
func chatgptExplain(status int, body []byte) string {
	code, param := chatgptErrorCode(body)
	switch {
	case code == "subscription_sharing_usage_limit_exceeded":
		return "the ChatGPT plan's usage limit, shared with ChatGPT and the other apps it is used in, is reached, see " + chatgptUsageURL
	case code == "subscription_sharing_usage_unavailable", code == "subscription_sharing_user_unavailable":
		return "OpenAI can't tell the account's usage right now, try again in a moment"
	case code == "subscription_sharing_user_not_eligible":
		return "this ChatGPT account can't be used through OpenAI's API, its plan isn't eligible"
	case code == "subscription_sharing_unsupported_capability":
		if param != "" {
			return "a ChatGPT sign-in can't use " + param + " through OpenAI's API"
		}
		return "a ChatGPT sign-in can't use part of this request through OpenAI's API"
	case code == "subscription_sharing_route_not_supported":
		return "a ChatGPT sign-in can't use this endpoint of OpenAI's API"
	case code == "subscription_sharing_invalid_user":
		return "OpenAI no longer takes this ChatGPT sign-in, " + chatgptLoginHint + " to sign in again"
	case strings.HasPrefix(code, "chatpass_v2_"):
		return "ChatGPT refused the account (" + code + ")"
	case status == http.StatusUnauthorized:
		return "OpenAI turned the ChatGPT sign-in away, " + chatgptLoginHint + " to sign in again"
	}
	return ""
}
