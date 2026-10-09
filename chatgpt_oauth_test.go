package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testRedirect = "http://127.0.0.1:1455/auth/callback"
	testHost     = "urn:uuid:00000000-0000-4000-8000-000000000000"
)

// flow is a sign-in under way against auth, whose ID tokens will carry its
// nonce.
func flow(auth *fakeAuth) *loginFlow {
	f := newLoginFlow(testRedirect, testHost)
	auth.mu.Lock()
	auth.nonce = f.nonce
	auth.mu.Unlock()
	return f
}

const goodCallback = "/auth/callback?code=good-code&client_id=" + testClient + "&state="

func TestAuthorizeURL(t *testing.T) {
	old := chatgptAuthorizeURL
	chatgptAuthorizeURL = "https://auth.openai.com/api/accounts/authorize"
	defer func() { chatgptAuthorizeURL = old }()

	f := newLoginFlow(testRedirect, testHost)
	u, err := url.Parse(f.authorizeURL())
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != "https://auth.openai.com/api/accounts/authorize" {
		t.Errorf("endpoint = %s", got)
	}
	sum := sha256.Sum256([]byte(f.verifier))
	want := map[string]string{
		"client_id":             "dynamic_agent_client",
		"agent_name_hint":       "cameo",
		"ext_agent_host_id":     testHost,
		"response_type":         "code",
		"redirect_uri":          testRedirect,
		"resource":              "https://api.openai.com/v1",
		"scope":                 "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct",
		"state":                 f.state,
		"nonce":                 f.nonce,
		"code_challenge":        base64.RawURLEncoding.EncodeToString(sum[:]),
		"code_challenge_method": "S256",
	}
	q := u.Query()
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if len(q) != len(want) {
		t.Errorf("parameters = %v", q)
	}
	// RFC 7636 wants 43 to 128 characters of verifier.
	if n := len(f.verifier); n < 43 || n > 128 || len(f.state) < 16 || len(f.nonce) < 16 {
		t.Errorf("verifier of %d, state of %d, nonce of %d characters", n, len(f.state), len(f.nonce))
	}
	g := newLoginFlow(testRedirect, testHost)
	if g.verifier == f.verifier || g.state == f.state || g.nonce == f.nonce || f.state == f.nonce {
		t.Error("secrets are shared")
	}
}

// The machine's id is made once and is the same from then on.
func TestHostID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cameo")
	id, err := hostID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Errorf("id = %q", id)
	}
	if again, err := hostID(dir); err != nil || again != id {
		t.Errorf("then %q, %v", again, err)
	}
	if other, _ := hostID(t.TempDir()); other == id {
		t.Error("two machines share an id")
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(filepath.Join(dir, "chatgpt-host")); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("file: %v, %v", st, err)
		}
	}
	// A file that is not an id is replaced.
	os.WriteFile(filepath.Join(dir, "chatgpt-host"), []byte("nonsense"), 0o600)
	if fixed, err := hostID(dir); err != nil || !strings.HasPrefix(fixed, "urn:uuid:") {
		t.Errorf("after nonsense: %q, %v", fixed, err)
	}
}

// get asks the callback handler for path the way the browser would.
func get(f *loginFlow, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func outcome(f *loginFlow) (loginResult, bool) {
	select {
	case res := <-f.done:
		return res, true
	default:
		return loginResult{}, false
	}
}

func TestCallback(t *testing.T) {
	auth := newFakeAuth(t)

	t.Run("state mismatch", func(t *testing.T) {
		f := flow(auth)
		for _, query := range []string{"code=good-code&client_id=oaiapp_1&state=someone-elses", "code=good-code&client_id=oaiapp_1", "client_id=oaiapp_1&state=" + f.state} {
			if rec := get(f, "/auth/callback?"+query); rec.Code != http.StatusBadRequest {
				t.Errorf("%s: status %d", query, rec.Code)
			}
		}
		if _, over := outcome(f); over {
			t.Error("the sign-in ended")
		}
		if _, exchanges, _ := auth.counts(); exchanges != 0 {
			t.Error("a code was traded")
		}
	})

	t.Run("error", func(t *testing.T) {
		f := flow(auth)
		rec := get(f, "/auth/callback?error=access_denied&error_description=The+user+said+%3Cno%3E&state="+f.state)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "The user said &lt;no&gt;") {
			t.Errorf("status %d: %s", rec.Code, rec.Body)
		}
		res, over := outcome(f)
		if !over || res.err == nil || res.err.Error() != "The user said <no>" {
			t.Errorf("outcome %+v, over %v", res, over)
		}
		f = flow(auth)
		get(f, "/auth/callback?error=access_denied")
		if res, _ := outcome(f); res.err == nil || res.err.Error() != "access_denied" {
			t.Errorf("outcome %+v", res)
		}
	})

	t.Run("success", func(t *testing.T) {
		f := flow(auth)
		rec := get(f, goodCallback+f.state)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "signed in") {
			t.Errorf("status %d: %s", rec.Code, rec.Body)
		}
		res, over := outcome(f)
		if !over || res.err != nil || res.in.Email != "user@example.com" || res.in.ClientID != testClient {
			t.Fatalf("outcome %+v, over %v", res, over)
		}
		// The same address once more, from a reload or pasted as well.
		_, before, _ := auth.counts()
		if rec := get(f, goodCallback+f.state); rec.Code != http.StatusBadRequest {
			t.Errorf("second time: status %d", rec.Code)
		}
		if _, exchanges, _ := auth.counts(); exchanges != before {
			t.Errorf("the code was traded %d more times", exchanges-before)
		}
	})

	t.Run("no client id", func(t *testing.T) {
		f := flow(auth)
		_, before, _ := auth.counts()
		rec := get(f, "/auth/callback?code=good-code&state="+f.state)
		res, _ := outcome(f)
		if rec.Code != http.StatusBadRequest || res.err == nil || !strings.Contains(res.err.Error(), "no client id") {
			t.Errorf("status %d, outcome %+v", rec.Code, res)
		}
		if _, exchanges, _ := auth.counts(); exchanges != before {
			t.Error("a code was traded with no client id")
		}
	})

	t.Run("refused code", func(t *testing.T) {
		f := flow(auth)
		rec := get(f, "/auth/callback?code=bad-code&client_id=oaiapp_1&state="+f.state)
		res, _ := outcome(f)
		if rec.Code != http.StatusBadRequest || res.err == nil || !strings.Contains(res.err.Error(), "invalid_grant the code is not good") {
			t.Errorf("status %d, outcome %+v", rec.Code, res)
		}
	})

	t.Run("other paths", func(t *testing.T) {
		f := flow(auth)
		for _, path := range []string{"/callback", "/cancel", "/"} {
			if rec := get(f, path+"?code=good-code&client_id=oaiapp_1&state="+f.state); rec.Code != http.StatusNotFound {
				t.Errorf("%s: status %d", path, rec.Code)
			}
		}
		if _, over := outcome(f); over {
			t.Error("the sign-in ended")
		}
	})
}

func TestPastedCallback(t *testing.T) {
	auth := newFakeAuth(t)
	f := flow(auth)
	ctx := context.Background()
	const query = "?code=good-code&client_id=oaiapp_1&state="
	for name, addr := range map[string]string{
		"not an address":  "good-code",
		"no query":        "http://127.0.0.1:1455/auth/callback",
		"https":           "https://127.0.0.1:1455/auth/callback" + query + f.state,
		"another port":    "http://127.0.0.1:1456/auth/callback" + query + f.state,
		"no port":         "http://127.0.0.1/auth/callback" + query + f.state,
		"another host":    "http://example.com:1455/auth/callback" + query + f.state,
		"lookalike host":  "http://127.0.0.1.example.com:1455/auth/callback" + query + f.state,
		"another path":    "http://127.0.0.1:1455/callback" + query + f.state,
		"another state":   "http://127.0.0.1:1455/auth/callback" + query + "other",
		"no state":        "http://127.0.0.1:1455/auth/callback?code=good-code&client_id=oaiapp_1",
		"the sign-in URL": f.authorizeURL(),
	} {
		if err := f.pasted(ctx, addr); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	if _, over := outcome(f); over {
		t.Error("the sign-in ended")
	}
	if _, exchanges, _ := auth.counts(); exchanges != 0 {
		t.Error("a code was traded")
	}

	for _, host := range []string{"127.0.0.1", "localhost", "[::1]", "LOCALHOST"} {
		f := flow(auth)
		addr := "  http://" + host + ":1455/auth/callback" + query + f.state + "\n"
		if err := f.pasted(ctx, addr); err != nil {
			t.Errorf("%s: %v", host, err)
		}
		if res, over := outcome(f); !over || res.err != nil || res.in.Subject != "user-1" {
			t.Errorf("%s: outcome %+v", host, res)
		}
	}

	f = flow(auth)
	err := f.pasted(ctx, "http://127.0.0.1:1455/auth/callback?error=access_denied&state="+f.state)
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("err = %v", err)
	}
}

func TestExchangeCode(t *testing.T) {
	auth := newFakeAuth(t)
	f := flow(auth)
	in, err := exchangeCode(context.Background(), "good-code", testClient, f)
	if err != nil {
		t.Fatal(err)
	}
	// The code goes with the client id OpenAI issued, not the one the
	// browser was sent off with.
	form := auth.codes[0]
	want := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     testClient,
		"code":          "good-code",
		"code_verifier": f.verifier,
		"redirect_uri":  testRedirect,
		"resource":      "https://api.openai.com/v1",
	}
	for k, v := range want {
		if form.Get(k) != v {
			t.Errorf("%s = %q", k, form.Get(k))
		}
	}
	if len(form) != len(want) {
		t.Errorf("form = %v", form)
	}
	if in.ClientID != testClient || in.AccessToken != "access-1" || in.RefreshToken != "refresh-1" || in.IDToken == "" {
		t.Errorf("tokens = %+v", in)
	}
	if in.Subject != "user-1" || in.Email != "user@example.com" || in.Plan != "plus" || in.LastRefresh.IsZero() {
		t.Errorf("identity = %+v", in)
	}
	if left := time.Until(in.ExpiresAt); left < 59*time.Minute || left > 61*time.Minute {
		t.Errorf("expires in %v", left)
	}
	if strings.Join(in.Scopes, " ") != allScopes {
		t.Errorf("scopes = %v", in.Scopes)
	}
	if in.who() != "user@example.com (plus)" {
		t.Errorf("who = %q", in.who())
	}
	if got := (&signIn{Subject: "user-1"}).who(); got != "user-1" {
		t.Errorf("who = %q", got)
	}
}

// A sign-in OpenAI did not finish, one that does not grant the use of the
// plan through the API, or one whose ID token is not this sign-in's, is
// no sign-in.
func TestExchangeRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		client string
		setup  func(*fakeAuth)
		want   string
	}{
		"no client id":     {"", func(*fakeAuth) {}, "no client id"},
		"another client":   {"oaiapp_2", func(*fakeAuth) {}, "invalid_client"},
		"wrong verifier":   {testClient, func(f *fakeAuth) { f.challenge = "something-else" }, "does not fit the challenge"},
		"not eligible":     {testClient, func(f *fakeAuth) { f.scope = "openid profile email offline_access" }, "may not be eligible"},
		"no lifetime":      {testClient, func(f *fakeAuth) { f.expiresIn = 0 }, "no lifetime"},
		"no ID token":      {testClient, func(f *fakeAuth) { f.noID = true }, "no ID token"},
		"another nonce":    {testClient, func(f *fakeAuth) { f.nonce = "someone-elses" }, "isn't this sign-in's"},
		"another issuer":   {testClient, func(f *fakeAuth) { f.issuer = "https://evil.example.com" }, "not OpenAI"},
		"another audience": {testClient, func(f *fakeAuth) { f.audience = "oaiapp_2" }, "for another app"},
		"expired ID token": {testClient, func(f *fakeAuth) { f.idExpired = true }, "has expired"},
	} {
		t.Run(name, func(t *testing.T) {
			auth := newFakeAuth(t)
			f := flow(auth)
			tc.setup(auth)
			in, err := exchangeCode(context.Background(), "good-code", tc.client, f)
			if in != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%+v, err = %v", in, err)
			}
		})
	}
}

func TestIDTokenAudience(t *testing.T) {
	id := map[string]any{"iss": chatgptIssuer + "/", "aud": []any{"other", testClient}, "nonce": "n"}
	if err := checkIDToken(id, testClient, "n"); err != nil {
		t.Errorf("one audience of several: %v", err)
	}
	for _, tok := range []string{"", "a.b", "a.!!!.c", "a." + base64.RawURLEncoding.EncodeToString([]byte("[1]")) + ".c"} {
		if err := checkIDToken(jwtClaims(tok), testClient, "n"); err == nil {
			t.Errorf("%q passed", tok)
		}
	}
}

// With the usual port busy the sign-in takes another, and whatever holds
// the usual one is left alone.
func TestCallbackPortFallback(t *testing.T) {
	var asked atomic.Int32
	holder := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { asked.Add(1) }))
	defer holder.Close()
	old := chatgptCallbackAddr
	chatgptCallbackAddr = holder.Listener.Addr().String()
	defer func() { chatgptCallbackAddr = old }()

	ln, err := listenCallback()
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	ln.Close()
	if !addr.IP.IsLoopback() || addr.String() == chatgptCallbackAddr {
		t.Errorf("listening on %s", addr)
	}
	if asked.Load() != 0 {
		t.Error("the holder of the port was sent a request")
	}

	// Free, the usual one is taken.
	holder.Close()
	if ln, err = listenCallback(); err != nil || ln.Addr().String() != chatgptCallbackAddr {
		t.Errorf("listening on %v, %v", ln.Addr(), err)
	}
	ln.Close()
}

func TestRevoke(t *testing.T) {
	auth := newFakeAuth(t)
	if err := revoke(context.Background(), testSignIn(time1h())); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"token": "refresh-0", "token_type_hint": "refresh_token", "client_id": testClient}
	form := auth.revoked[0]
	for k, v := range want {
		if form.Get(k) != v {
			t.Errorf("%s = %q", k, form.Get(k))
		}
	}
	if len(form) != len(want) || len(auth.revoked) != 1 {
		t.Errorf("revoked %v", auth.revoked)
	}
	auth.noRevocation = true
	if err := revoke(context.Background(), testSignIn(time1h())); err == nil || !strings.Contains(err.Error(), "no revocation endpoint") {
		t.Errorf("err = %v", err)
	}
}
