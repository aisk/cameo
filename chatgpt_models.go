package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// chatgptModel is one entry of the account's model list.
type chatgptModel struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	Visibility  string `json:"visibility"`
	Levels      []struct {
		Effort string `json:"effort"`
	} `json:"supported_reasoning_levels"`
	ContextWindow int `json:"context_window"`
}

// listed says whether the model is one to offer, not one OpenAI keeps
// hidden.
func (m chatgptModel) listed() bool {
	return m.Slug != "" && (m.Visibility == "" || m.Visibility == "list")
}

func (m chatgptModel) efforts() []string {
	out := make([]string, 0, len(m.Levels))
	for _, l := range m.Levels {
		out = append(out, l.Effort)
	}
	return out
}

// models asks OpenAI which models the account can use, in its order.
func (a *chatgptAccount) models(ctx context.Context) ([]chatgptModel, error) {
	res, err := a.getModels(ctx)
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusUnauthorized {
		res.Body.Close()
		if _, err := a.refresh(ctx, true); err != nil {
			return nil, err
		}
		if res, err = a.getModels(ctx); err != nil {
			return nil, err
		}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode != http.StatusOK {
		msg := "ChatGPT models: " + errorMessage(raw, res.Status)
		if more := chatgptExplain(res.StatusCode, raw); more != "" {
			msg += " (" + more + ")"
		}
		return nil, errors.New(msg)
	}
	var list struct {
		Models []chatgptModel `json:"models"`
	}
	json.Unmarshal(raw, &list)
	var out []chatgptModel
	for _, m := range list.Models {
		if m.listed() {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("ChatGPT lists no models for this account")
	}
	return out, nil
}

func (a *chatgptAccount) getModels(ctx context.Context) (*http.Response, error) {
	who, err := a.token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(chatgptAPI, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+who.AccessToken)
	return authClient.Do(req)
}
