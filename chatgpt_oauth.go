package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// The sign-in is OpenAI's Sign in with ChatGPT, which lets an app running
// on the user's machine use the plan through OpenAI's own API. It registers
// cameo with OpenAI on the way, as a public client with no secret: the
// browser is sent to the authorize page as chatgptDynamicClient and comes
// back with the client id OpenAI issued next to the code. The code, every
// renewal and the sign-out go with that client id. It is ported from
// magpie's internal/provider/chatgpt_api.go.

// OpenAI's endpoints. Variables, so tests can point them elsewhere.
var (
	chatgptIssuer       = "https://auth.openai.com"
	chatgptAuthorizeURL = "https://auth.openai.com/api/accounts/authorize"
	chatgptTokenURL     = "https://auth.openai.com/api/accounts/oauth/token"
	// chatgptCallbackAddr is where the browser is sent back to first, as
	// with OpenAI's own apps. When it is busy another port is taken: only
	// the port of a loopback redirect may vary.
	chatgptCallbackAddr = "127.0.0.1:1455"
)

const (
	// chatgptResource is what the tokens are for.
	chatgptResource = "https://api.openai.com/v1"
	// chatgptDynamicClient registers the app on its first sign-in.
	chatgptDynamicClient = "dynamic_agent_client"
	chatgptAgentName     = "cameo"
	// chatgptDirectScope lets the token be sent to api.openai.com itself.
	chatgptDirectScope = "chatgpt.tokens.use.direct"
	chatgptScopes      = "openid profile email offline_access resource.invoke " + chatgptDirectScope
	// chatgptClaims is where OpenAI's tokens name the plan.
	chatgptClaims = "https://api.openai.com/auth"
	// chatgptUsageURL is where a ChatGPT account's usage is shown.
	chatgptUsageURL = "https://chatgpt.com/settings/usage"
)

// authClient talks to OpenAI's sign-in. A renewal is made holding the lock
// of the sign-in file, so it must not hang.
var authClient = &http.Client{Timeout: 30 * time.Second}

func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// hostID is this machine's id for OpenAI, a version 4 UUID made once and
// kept in the directory of the sign-in file.
func hostID(dir string) (string, error) {
	path := filepath.Join(dir, "chatgpt-host")
	if raw, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(raw)); strings.HasPrefix(id, "urn:uuid:") {
			return id, nil
		}
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b)
	id := "urn:uuid:" + h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return id, os.WriteFile(path, []byte(id+"\n"), 0o600)
}

// loginFlow is one sign-in under way: what the authorize URL was made
// with, and what the browser coming back is checked against.
type loginFlow struct {
	verifier string
	state    string
	nonce    string
	hostID   string
	redirect string
	// claim makes sure the code is traded once, whether the browser
	// brought it or its address was pasted.
	claim sync.Once
	// done gets the outcome, once.
	done chan loginResult
}

type loginResult struct {
	in  *signIn
	err error
}

func newLoginFlow(redirect, hostID string) *loginFlow {
	return &loginFlow{
		verifier: randomToken(48),
		state:    randomToken(24),
		nonce:    randomToken(24),
		hostID:   hostID,
		redirect: redirect,
		done:     make(chan loginResult, 1),
	}
}

func (f *loginFlow) authorizeURL() string {
	sum := sha256.Sum256([]byte(f.verifier))
	q := url.Values{}
	q.Set("client_id", chatgptDynamicClient)
	q.Set("agent_name_hint", chatgptAgentName)
	q.Set("ext_agent_host_id", f.hostID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", f.redirect)
	q.Set("resource", chatgptResource)
	q.Set("scope", chatgptScopes)
	q.Set("state", f.state)
	q.Set("nonce", f.nonce)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	q.Set("code_challenge_method", "S256")
	return chatgptAuthorizeURL + "?" + q.Encode()
}

// ServeHTTP is where the browser comes back to.
func (f *loginFlow) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/auth/callback" {
		http.NotFound(w, r)
		return
	}
	ok, title, msg := f.callback(r.Context(), r.URL.Query())
	signInPage(w, ok, title, msg)
}

// callback takes the query the browser was sent back with and, if it is
// this sign-in's, trades its code for the tokens. It answers what to tell
// the person at the browser.
func (f *loginFlow) callback(ctx context.Context, q url.Values) (ok bool, title, msg string) {
	if e := q.Get("error"); e != "" {
		msg := q.Get("error_description")
		if msg == "" {
			msg = e
		}
		f.finish(nil, errors.New(msg))
		return false, "Sign-in didn't finish", msg
	}
	if q.Get("state") != f.state || q.Get("code") == "" {
		// Not ours: someone else's page, or a stale tab. The sign-in goes
		// on waiting for its own.
		return false, "This link isn't from cameo's sign-in", "Start it again with cameo provider login chatgpt."
	}
	claimed := false
	f.claim.Do(func() { claimed = true })
	if !claimed {
		return false, "This sign-in is already finishing", "Look at the terminal cameo runs in."
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	in, err := exchangeCode(ctx, q.Get("code"), strings.TrimSpace(q.Get("client_id")), f)
	f.finish(in, err)
	if err != nil {
		return false, "Sign-in didn't finish", err.Error()
	}
	return true, "You're signed in", "You can close this tab and go back to the terminal."
}

func (f *loginFlow) finish(in *signIn, err error) {
	select {
	case f.done <- loginResult{in, err}:
	default:
	}
}

// pasted takes the address the browser was sent back to, for a cameo that
// browser cannot reach. Only this sign-in's own address is taken: its port
// and path on this machine, and its state.
func (f *loginFlow) pasted(ctx context.Context, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "http" || u.RawQuery == "" {
		return errors.New("paste the whole address the browser ended on, starting with http://")
	}
	if !f.ownCallback(u) {
		return errors.New("that address isn't from this sign-in: paste the one its browser tab ended on")
	}
	if ok, _, msg := f.callback(ctx, u.Query()); !ok {
		return errors.New(msg)
	}
	return nil
}

func (f *loginFlow) ownCallback(u *url.URL) bool {
	want, err := url.Parse(f.redirect)
	if err != nil || u.Port() != want.Port() || u.Path != want.Path {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "localhost", "127.0.0.1", "::1":
	default:
		return false
	}
	return u.Query().Get("state") == f.state
}

func signInPage(w http.ResponseWriter, ok bool, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
	}
	fmt.Fprintf(w, "<!doctype html><html><head><meta charset=\"utf-8\"><title>cameo: %[1]s</title></head>"+
		"<body style=\"font-family:system-ui,sans-serif;text-align:center;margin-top:20vh\">"+
		"<h1>%[1]s</h1><p>%[2]s</p></body></html>\n", html.EscapeString(title), html.EscapeString(msg))
}

// listenCallback takes the port the browser is sent back to: the usual one,
// or any other when that one is busy, with a Codex sign-in waiting there
// for one.
func listenCallback() (net.Listener, error) {
	if ln, err := net.Listen("tcp", chatgptCallbackAddr); err == nil {
		return ln, nil
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// tokenReply is the token endpoint's answer.
type tokenReply struct {
	Access    string  `json:"access_token"`
	Refresh   string  `json:"refresh_token"`
	IDToken   string  `json:"id_token"`
	ExpiresIn float64 `json:"expires_in"`
	Scope     string  `json:"scope"`
}

// tokenError is the token endpoint's refusal, with its OAuth code.
type tokenError struct {
	status int
	code   string
	msg    string
}

func (e *tokenError) Error() string {
	code := e.code
	if code == "" {
		code = http.StatusText(e.status)
	}
	return strings.TrimSpace(code + " " + e.msg)
}

func postToken(ctx context.Context, form url.Values) (*tokenReply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := authClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return nil, refusal(raw, res)
	}
	var tok tokenReply
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("unreadable token answer: %w", err)
	}
	return &tok, nil
}

// refusal reads why the token endpoint said no. Its error is an OAuth code
// with a description, or an object with a code and a message.
func refusal(raw []byte, res *http.Response) *tokenError {
	var e struct {
		Error any    `json:"error"`
		Desc  string `json:"error_description"`
		Code  string `json:"code"`
	}
	json.Unmarshal(raw, &e)
	out := &tokenError{status: res.StatusCode, code: e.Code, msg: e.Desc}
	first := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	switch v := e.Error.(type) {
	case string:
		out.code = first(out.code, v)
	case map[string]any:
		code, _ := v["code"].(string)
		msg, _ := v["message"].(string)
		out.code, out.msg = first(out.code, code), first(out.msg, msg)
	}
	if out.code == "" && out.msg == "" {
		out.msg = errorMessage(raw, res.Status)
	}
	return out
}

// exchangeCode trades the code the browser came back with for the tokens,
// under the client id OpenAI issued and sent back next to it.
func exchangeCode(ctx context.Context, code, clientID string, f *loginFlow) (*signIn, error) {
	if clientID == "" {
		return nil, errors.New("OpenAI didn't finish registering cameo (no client id came back), start the sign-in again")
	}
	tok, err := postToken(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"code_verifier": {f.verifier},
		"redirect_uri":  {f.redirect},
		"resource":      {chatgptResource},
	})
	if err != nil {
		return nil, fmt.Errorf("the sign-in was refused: %w", err)
	}
	if tok.IDToken == "" {
		return nil, errors.New("OpenAI sent no ID token")
	}
	in, err := (&signIn{ClientID: clientID}).withTokens(tok)
	if err != nil {
		return nil, err
	}
	id := jwtClaims(tok.IDToken)
	if err := checkIDToken(id, clientID, f.nonce); err != nil {
		return nil, err
	}
	in.Subject, in.Email = claimString(id, "sub"), claimString(id, "email")
	if in.Subject == "" && in.Email == "" {
		return nil, errors.New("OpenAI didn't say which account signed in")
	}
	return in, nil
}

// withTokens is the sign-in with the tokens of a token answer in it. An
// answer that does not grant the use of the plan through the API is no
// sign-in at all.
func (in *signIn) withTokens(tok *tokenReply) (*signIn, error) {
	if tok.Access == "" || tok.Refresh == "" {
		return nil, errors.New("OpenAI sent no token")
	}
	if tok.ExpiresIn <= 0 {
		return nil, errors.New("OpenAI's token has no lifetime")
	}
	scopes := strings.Fields(tok.Scope)
	if !slices.Contains(scopes, chatgptDirectScope) {
		return nil, errors.New("OpenAI didn't grant cameo the use of the subscription through its API (" + chatgptDirectScope + "): this ChatGPT account may not be eligible")
	}
	next := *in
	next.AccessToken, next.RefreshToken, next.Scopes = tok.Access, tok.Refresh, scopes
	now := time.Now().UTC()
	next.ExpiresAt = now.Add(time.Duration(tok.ExpiresIn * float64(time.Second)))
	next.LastRefresh = now
	if tok.IDToken != "" {
		next.IDToken = tok.IDToken
	}
	for _, t := range []string{next.IDToken, next.AccessToken} {
		if plan := claimString(jwtClaims(t), chatgptClaims, "chatgpt_plan_type"); plan != "" {
			next.Plan = plan
			break
		}
	}
	return &next, nil
}

// checkIDToken checks the ID token is this sign-in's: OpenAI's, for the
// client it issued, with the nonce the sign-in sent, not expired. Its
// signature is not checked: it came straight from OpenAI over TLS.
func checkIDToken(id map[string]any, clientID, nonce string) error {
	if id == nil {
		return errors.New("OpenAI sent an unreadable ID token")
	}
	if iss := claimString(id, "iss"); strings.TrimRight(iss, "/") != strings.TrimRight(chatgptIssuer, "/") {
		return fmt.Errorf("the ID token is from %q, not OpenAI", iss)
	}
	aud := false
	switch v := id["aud"].(type) {
	case string:
		aud = v == clientID
	case []any:
		aud = slices.Contains(v, any(clientID))
	}
	if !aud {
		return errors.New("the ID token is for another app")
	}
	if claimString(id, "nonce") != nonce {
		return errors.New("the ID token isn't this sign-in's")
	}
	if exp, ok := id["exp"].(float64); ok && time.Unix(int64(exp), 0).Before(time.Now().Add(-time.Minute)) {
		return errors.New("the ID token has expired")
	}
	return nil
}

// deadRefresh are the refusals of a refresh token that will never work
// again.
var deadRefresh = []string{"invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_expired",
	"refresh_token_invalidated", "refresh_token_reused"}

// refreshTokens renews a sign-in. OpenAI hands out a new refresh token
// with each renewal and stops taking the old one, so the caller must hold
// the lock of the sign-in file and keep what comes back. For the same
// reason the renewal is not given up when ctx is: an answer nobody reads
// would lose the sign-in.
func refreshTokens(ctx context.Context, cur *signIn) (*signIn, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	tok, err := postToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {cur.ClientID},
		"refresh_token": {cur.RefreshToken},
		"resource":      {chatgptResource},
	})
	var te *tokenError
	switch {
	case errors.As(err, &te) && slices.Contains(deadRefresh, te.code):
		return nil, errSignInGone
	case errors.As(err, &te) && te.code == "invalid_client":
		return nil, errClientGone
	case err != nil:
		return nil, fmt.Errorf("ChatGPT token refresh: %w", err)
	}
	next, err := cur.withTokens(tok)
	if err != nil {
		return nil, fmt.Errorf("ChatGPT token refresh: %w", err)
	}
	return next, nil
}

// revoke has OpenAI revoke a sign-in's refresh token, at the revocation
// endpoint its discovery document names.
func revoke(ctx context.Context, in *signIn) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(chatgptIssuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return err
	}
	res, err := authClient.Do(req)
	if err != nil {
		return err
	}
	var doc struct {
		Revoke string `json:"revocation_endpoint"`
	}
	err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&doc)
	res.Body.Close()
	if err != nil || doc.Revoke == "" {
		return errors.New("OpenAI names no revocation endpoint")
	}
	form := url.Values{"token": {in.RefreshToken}, "token_type_hint": {"refresh_token"}, "client_id": {in.ClientID}}
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, doc.Revoke, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if res, err = authClient.Do(req); err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("the revocation endpoint answered %s", res.Status)
	}
	return nil
}

// who names the account for a person to read.
func (in *signIn) who() string {
	name := in.Email
	if name == "" {
		name = in.Subject
	}
	if in.Plan == "" {
		return name
	}
	return name + " (" + in.Plan + ")"
}

// jwtClaims decodes the payload of a JWT without checking its signature.
func jwtClaims(tok string) map[string]any {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	return claims
}

func claimString(claims map[string]any, keys ...string) string {
	var v any = claims
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = m[k]
	}
	s, _ := v.(string)
	return s
}
