package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"github.com/aisk/cameo/llmconv"
)

// chatgptProvider is the built-in provider serving agents through the
// user's ChatGPT subscription: the access token of a Sign in with ChatGPT
// is sent to OpenAI's own Responses API, with the agent's own instructions
// and nothing said of who is asking but the token.
const chatgptProvider = "chatgpt"

// chatgptAPI is where the requests go. A variable, so tests can point it
// elsewhere.
var chatgptAPI = "https://api.openai.com/v1"

type chatgptBackend struct {
	account *chatgptAccount
}

func newChatGPTBackend(store *authStore) *chatgptBackend {
	return &chatgptBackend{account: &chatgptAccount{store: store}}
}

func (b *chatgptBackend) api() llmconv.Protocol { return llmconv.Responses }

// host is fixed, wherever chatgptAPI points: it is what llmconv knows
// OpenAI's API by.
func (b *chatgptBackend) host() string { return "api.openai.com" }

func (b *chatgptBackend) request(ctx context.Context, model string, body []byte) (*http.Request, error) {
	who, err := b.account.token(ctx)
	if err != nil {
		return nil, err
	}
	u := strings.TrimRight(chatgptAPI, "/") + "/responses"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(chatgptBody(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+who.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	return req, nil
}

// unauthorized renews the tokens OpenAI turned away. When that fails the
// 401 stands, and upstreamError says what to do about it.
func (b *chatgptBackend) unauthorized(ctx context.Context) bool {
	_, err := b.account.refresh(ctx, true)
	return err == nil
}
