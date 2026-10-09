package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// provider runs `cameo provider args...` with stdin and returns what it
// printed.
func provider(t *testing.T, stdin io.Reader, args ...string) (stdout, stderr string, code int, err error) {
	t.Helper()
	var out, errOut lockedBuffer
	code, err = providerCommand(args, stdin, &out, &errOut)
	return out.String(), errOut.String(), code, err
}

func TestProviderUsage(t *testing.T) {
	tempAuth(t)
	for _, args := range [][]string{
		nil,
		{"help"},
		{"signin", "chatgpt"},
		{"login"},
		{"logout"},
		{"models"},
		{"list", "chatgpt"},
		{"login", "chatgpt", "again"},
	} {
		stdout, stderr, code, err := provider(t, strings.NewReader(""), args...)
		if code == 0 || err != nil || stdout != "" {
			t.Errorf("%q: code %d, err %v, stdout %q", args, code, err, stdout)
		}
		for _, want := range []string{"cameo provider login <name>", "cameo provider logout <name>", "cameo provider list", "cameo provider models <name>"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("%q: usage lacks %q: %s", args, want, stderr)
			}
		}
	}
}

// browser stands in for the user's browser: given the sign-in URL, it does
// what visit says with it.
func browser(t *testing.T, visit func(signInURL *url.URL)) {
	t.Helper()
	old := openBrowser
	openBrowser = func(raw string) {
		u, err := url.Parse(raw)
		if err != nil {
			t.Error(err)
			return
		}
		go visit(u)
	}
	t.Cleanup(func() { openBrowser = old })
}

// visit is what OpenAI does with the browser: note what the sign-in asked
// for, then send it back with a code and the client id it issued.
func visit(t *testing.T, auth *fakeAuth, u *url.URL) (back string) {
	q := u.Query()
	auth.mu.Lock()
	auth.challenge, auth.nonce = q.Get("code_challenge"), q.Get("nonce")
	auth.mu.Unlock()
	return "?code=good-code&client_id=" + testClient + "&state=" + q.Get("state")
}

func TestProviderLogin(t *testing.T) {
	auth := newFakeAuth(t)
	path := tempAuth(t)
	t.Setenv("CAMEO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	var page, host atomic.Value
	browser(t, func(u *url.URL) {
		host.Store(u.Query().Get("ext_agent_host_id"))
		resp, err := http.Get(u.Query().Get("redirect_uri") + visit(t, auth, u))
		if err != nil {
			t.Error(err)
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		page.Store(string(body))
	})

	// stdin stays open and silent, as a terminal nobody types at.
	stdin, hold := io.Pipe()
	defer hold.Close()
	stdout, _, code, err := provider(t, stdin, "login", "chatgpt")
	if code != 0 || err != nil {
		t.Fatalf("code %d, err %v: %s", code, err, stdout)
	}
	if !strings.Contains(stdout, "\n"+chatgptAuthorizeURL+"?") || !strings.Contains(stdout, "redirect_uri=http%3A%2F%2F127.0.0.1%3A") {
		t.Errorf("no sign-in URL in: %s", stdout)
	}
	if !strings.Contains(stdout, "Signed in to chatgpt as user@example.com (plus).\n") || !strings.Contains(stdout, "https://chatgpt.com/settings/usage") {
		t.Errorf("stdout = %s", stdout)
	}
	if lower := strings.ToLower(stdout); strings.Contains(lower, "unofficial") || strings.Contains(lower, "codex") {
		t.Errorf("stdout = %s", stdout)
	}
	if got, _ := page.Load().(string); !strings.Contains(got, "signed in") {
		t.Errorf("the browser was shown %q", got)
	}
	saved := readAuthFile(t, path)[chatgptProvider]
	for k, want := range map[string]any{
		"client_id": testClient, "access_token": "access-1", "refresh_token": "refresh-1",
		"subject": "user-1", "email": "user@example.com", "plan": "plus",
	} {
		if saved[k] != want {
			t.Errorf("saved %s = %v", k, saved[k])
		}
	}
	if saved["id_token"] == "" || saved["expires_at"] == "" || saved["last_refresh"] == "" || len(saved["scopes"].([]any)) != 6 {
		t.Errorf("saved %v", saved)
	}
	// The verifier sent with the code fit the challenge the browser carried.
	if _, exchanges, _ := auth.counts(); exchanges != 1 {
		t.Errorf("%d code exchanges", exchanges)
	}

	// The machine's id is kept next to the sign-ins, and used again.
	kept, err := os.ReadFile(filepath.Join(filepath.Dir(path), "chatgpt-host"))
	first, _ := host.Load().(string)
	if err != nil || strings.TrimSpace(string(kept)) != first || !strings.HasPrefix(first, "urn:uuid:") {
		t.Errorf("host id %q, kept %q, %v", first, kept, err)
	}
	if _, _, _, err := provider(t, stdin, "login", "chatgpt"); err != nil {
		t.Fatal(err)
	}
	if again, _ := host.Load().(string); again != first {
		t.Errorf("host id %q, then %q", first, again)
	}
}

// With port 1455 taken, by a Codex sign-in for one, another is used.
func TestProviderLoginPortBusy(t *testing.T) {
	auth := newFakeAuth(t)
	tempAuth(t)
	holder := newLocalListener(t)
	old := chatgptCallbackAddr
	chatgptCallbackAddr = holder
	defer func() { chatgptCallbackAddr = old }()
	browser(t, func(u *url.URL) {
		back := u.Query().Get("redirect_uri")
		if r, _ := url.Parse(back); r.Hostname() != "127.0.0.1" || r.Host == holder || r.Path != "/auth/callback" {
			t.Errorf("redirect_uri = %s", back)
		}
		if resp, err := http.Get(back + visit(t, auth, u)); err == nil {
			resp.Body.Close()
		}
	})
	stdin, hold := io.Pipe()
	defer hold.Close()
	if stdout, _, _, err := provider(t, stdin, "login", "chatgpt"); err != nil || !strings.Contains(stdout, "Signed in to chatgpt") {
		t.Errorf("err %v: %s", err, stdout)
	}
}

// On a machine the browser cannot reach, the address it ended on is typed
// in instead.
func TestProviderLoginPasted(t *testing.T) {
	auth := newFakeAuth(t)
	path := tempAuth(t)
	t.Setenv("CAMEO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	stdin, typed := io.Pipe()
	defer typed.Close()
	browser(t, func(u *url.URL) {
		back := visit(t, auth, u)
		io.WriteString(typed, "\nhttp://127.0.0.1:1/auth/callback"+back+"\n")
		io.WriteString(typed, u.Query().Get("redirect_uri")+back+"\n")
	})

	stdout, _, code, err := provider(t, stdin, "login", "chatgpt")
	if code != 0 || err != nil {
		t.Fatalf("code %d, err %v: %s", code, err, stdout)
	}
	if !strings.Contains(stdout, "that address isn't from this sign-in") {
		t.Errorf("the wrong address went unremarked: %s", stdout)
	}
	if !strings.Contains(stdout, "Signed in to chatgpt as user@example.com (plus).") {
		t.Errorf("stdout = %s", stdout)
	}
	if saved := readAuthFile(t, path)[chatgptProvider]; saved["refresh_token"] != "refresh-1" {
		t.Errorf("saved %v", saved)
	}
}

func TestProviderLoginFails(t *testing.T) {
	auth := newFakeAuth(t)
	path := tempAuth(t)
	t.Setenv("CAMEO_CONFIG", writeFile(t, testProvider+agent("third")))
	closed := strings.NewReader("")

	browser(t, func(u *url.URL) {
		q := u.Query()
		resp, err := http.Get(q.Get("redirect_uri") + "?error=access_denied&error_description=You+said+no&state=" + q.Get("state"))
		if err == nil {
			resp.Body.Close()
		}
	})
	if _, _, _, err := provider(t, closed, "login", "chatgpt"); err == nil || !strings.Contains(err.Error(), "You said no") {
		t.Errorf("denied: err = %v", err)
	}

	// An account whose plan may not use the API is told so, and not kept.
	auth.scope = "openid profile email offline_access"
	browser(t, func(u *url.URL) {
		if resp, err := http.Get(u.Query().Get("redirect_uri") + visit(t, auth, u)); err == nil {
			resp.Body.Close()
		}
	})
	if _, _, _, err := provider(t, closed, "login", "chatgpt"); err == nil || !strings.Contains(err.Error(), "may not be eligible") {
		t.Errorf("not eligible: err = %v", err)
	}

	browser(t, func(*url.URL) {})
	old := signInTimeout
	signInTimeout = 50 * time.Millisecond
	defer func() { signInTimeout = old }()
	if _, _, _, err := provider(t, closed, "login", "chatgpt"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("nobody came: err = %v", err)
	}

	for name, want := range map[string]string{
		"antigravity": "not supported yet",
		"third":       "needs no sign-in",
		"nowhere":     "unknown provider",
	} {
		for _, sub := range []string{"login", "logout"} {
			stdout, _, _, err := provider(t, closed, sub, name)
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), name) || stdout != "" {
				t.Errorf("%s %s: err = %v, stdout %q", sub, name, err, stdout)
			}
		}
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a sign-in was saved")
	}
}

func TestProviderLogout(t *testing.T) {
	auth := newFakeAuth(t)
	path := tempAuth(t)
	saveSignIn(t, testSignIn(time1h()))
	stdout, _, code, err := provider(t, nil, "logout", "chatgpt")
	if code != 0 || err != nil || stdout != "Signed out of chatgpt.\n" {
		t.Errorf("code %d, err %v: %q", code, err, stdout)
	}
	if entries := readAuthFile(t, path); len(entries) != 0 {
		t.Errorf("left %v", entries)
	}
	// OpenAI was told, under the client id of the sign-in.
	if len(auth.revoked) != 1 || auth.revoked[0].Get("token") != "refresh-0" || auth.revoked[0].Get("client_id") != testClient ||
		auth.revoked[0].Get("token_type_hint") != "refresh_token" {
		t.Errorf("revoked %v", auth.revoked)
	}

	stdout, _, _, err = provider(t, nil, "logout", "chatgpt")
	if err != nil || stdout != "chatgpt was not signed in.\n" || len(auth.revoked) != 1 {
		t.Errorf("again: err %v: %q, revoked %v", err, stdout, auth.revoked)
	}
}

// OpenAI out of reach does not keep the sign-in on this machine.
func TestProviderLogoutOffline(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T){
		"no revocation endpoint": func(t *testing.T) { newFakeAuth(t).noRevocation = true },
		"unreachable":            func(t *testing.T) {},
		"slow": func(t *testing.T) {
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
			t.Cleanup(srv.Close)
			t.Cleanup(func() { close(release) })
			old, oldTimeout := chatgptIssuer, revokeTimeout
			chatgptIssuer, revokeTimeout = srv.URL, 50*time.Millisecond
			t.Cleanup(func() { chatgptIssuer, revokeTimeout = old, oldTimeout })
		},
	} {
		t.Run(name, func(t *testing.T) {
			setup(t)
			path := tempAuth(t)
			saveSignIn(t, testSignIn(time1h()))
			stdout, _, code, err := provider(t, nil, "logout", "chatgpt")
			if code != 0 || err != nil || !strings.HasPrefix(stdout, "Signed out of chatgpt.\n") || !strings.Contains(stdout, "could not be asked to revoke it") {
				t.Errorf("code %d, err %v: %q", code, err, stdout)
			}
			if entries := readAuthFile(t, path); len(entries) != 0 {
				t.Errorf("left %v", entries)
			}
		})
	}
}

func TestProviderList(t *testing.T) {
	tempAuth(t)
	// An agent waits for a sign-in there is not: the list is shown anyway.
	t.Setenv("CAMEO_TEST_KEY", "sk-very-secret")
	t.Setenv("CAMEO_CONFIG", writeFile(t, `
[providers.third]
url = "https://example.com/anthropic"
key = "$CAMEO_TEST_KEY"

[providers.openai]
api = "responses"
url = "https://api.openai.com/v1"
key = "$CAMEO_TEST_KEY"
`+agent("chatgpt")))

	stdout, _, code, err := provider(t, nil, "list")
	if code != 0 || err != nil {
		t.Fatalf("code %d, err %v", code, err)
	}
	var rows [][]string
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		rows = append(rows, strings.Fields(line))
	}
	want := [][]string{
		{"NAME", "API", "DETAILS"},
		{"chatgpt", "responses", "not", "signed", "in"},
		{"openai", "responses", "https://api.openai.com/v1"},
		{"third", "anthropic", "https://example.com/anthropic"},
	}
	if len(rows) != len(want) {
		t.Fatalf("list = %s", stdout)
	}
	for i := range want {
		if strings.Join(rows[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("row %d = %q, want %q", i, rows[i], want[i])
		}
	}
	if strings.Contains(stdout, "sk-very-secret") {
		t.Error("a key was shown")
	}

	saveSignIn(t, testSignIn(time1h()))
	if stdout, _, _, _ := provider(t, nil, "list"); !strings.Contains(stdout, "signed in as user@example.com (plus)") {
		t.Errorf("signed in: %s", stdout)
	}

	// No config at all still has the built-in ones, and a broken one says so.
	t.Setenv("CAMEO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	if stdout, _, _, err := provider(t, nil, "list"); err != nil || strings.Count(stdout, "\n") != 2 || !strings.Contains(stdout, "chatgpt") {
		t.Errorf("no config: err %v: %s", err, stdout)
	}
	t.Setenv("CAMEO_CONFIG", writeFile(t, "upstream = "))
	if _, _, _, err := provider(t, nil, "list"); err == nil {
		t.Error("a broken config went unremarked")
	}
}

func TestProviderModels(t *testing.T) {
	tempAuth(t)
	t.Setenv("CAMEO_CONFIG", writeFile(t, testProvider+agent("third")))
	saveSignIn(t, testSignIn(time1h()))
	var got captured
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		got = captured{path: r.URL.Path, query: r.URL.RawQuery, header: r.Header}
		io.WriteString(w, modelList)
	})

	stdout, _, code, err := provider(t, nil, "models", "chatgpt")
	if code != 0 || err != nil {
		t.Fatalf("code %d, err %v", code, err)
	}
	var rows []string
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		rows = append(rows, strings.Join(strings.Fields(line), " "))
	}
	// In OpenAI's order, the hidden one left out, and a word on the rest.
	want := []string{"MODEL NAME REASONING", "gpt-5.5 GPT-5.5 low, medium, high", "gpt-5.4-mini GPT-5.4 mini low, medium",
		"The sign-in may also run newer models that this list leaves out."}
	if strings.Join(rows, "\n") != strings.Join(want, "\n") {
		t.Errorf("models:\n%s", stdout)
	}
	if got.path != "/v1/models" || got.query != "" || got.header.Get("Authorization") != "Bearer access-0" {
		t.Errorf("asked for %s?%s as %q", got.path, got.query, got.header.Get("Authorization"))
	}
	for _, name := range []string{"Originator", "Chatgpt-Account-Id", "Version", "Openai-Beta", "Session_id"} {
		if v := got.header.Get(name); v != "" {
			t.Errorf("header %s = %q", name, v)
		}
	}

	for name, want := range map[string]string{"third": "not supported for provider", "antigravity": "not supported yet"} {
		if _, _, _, err := provider(t, nil, "models", name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestProviderModelsProblems(t *testing.T) {
	tempAuth(t)
	if _, _, _, err := provider(t, nil, "models", "chatgpt"); err == nil || !strings.Contains(err.Error(), "cameo provider login chatgpt") {
		t.Errorf("signed out: err = %v", err)
	}

	// A token OpenAI turns away is renewed and the list asked for again.
	newFakeAuth(t)
	saveSignIn(t, testSignIn(time1h()))
	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, modelList)
	})
	if stdout, _, _, err := provider(t, nil, "models", "chatgpt"); err != nil || !strings.Contains(stdout, "gpt-5.4-mini") {
		t.Errorf("after a 401: err %v: %s", err, stdout)
	}

	fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"code":"subscription_sharing_user_not_eligible","message":"Your plan has none"}}`)
	})
	var out bytes.Buffer
	if _, err := providerCommand([]string{"models", "chatgpt"}, nil, &out, io.Discard); err == nil || !strings.Contains(err.Error(), "Your plan has none (") || !strings.Contains(err.Error(), "isn't eligible") || out.Len() != 0 {
		t.Errorf("refused: err = %v, stdout %q", err, out.String())
	}
}
