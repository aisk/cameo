package main

import (
	"bytes"
	"context"
	"errors"
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

// unauthorized renews the tokens OpenAI turned away. When OpenAI refuses
// that too the 401 stands, and upstreamError says what to do about it. A
// renewal that did not get through is no word on the sign-in, and is the
// failure to report.
func (b *chatgptBackend) unauthorized(ctx context.Context) (bool, error) {
	_, err := b.account.refresh(ctx, true)
	var se *statusError
	if err != nil && !errors.As(err, &se) {
		return false, err
	}
	return err == nil, nil
}
