package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// jwt is an unsigned token carrying claims.
func jwt(claims map[string]any) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "none"}) + "." + enc(claims) + ".sig"
}

func time1h() time.Time { return time.Now().Add(time.Hour) }

// testClient is the client id the fake OpenAI issues.
const testClient = "oaiapp_1"

const allScopes = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"

// testSignIn is a sign-in whose access token expires at exp, last renewed
// long enough ago for a 401 to renew it.
func testSignIn(exp time.Time) *signIn {
	return &signIn{
		ClientID:     testClient,
		AccessToken:  "access-0",
		RefreshToken: "refresh-0",
		IDToken:      "id-0",
		ExpiresAt:    exp.UTC(),
		Scopes:       []string{"openid", chatgptDirectScope},
		Subject:      "user-1",
		Email:        "user@example.com",
		Plan:         "plus",
		LastRefresh:  time.Now().Add(-time.Hour).UTC(),
	}
}

// tempAuth points CAMEO_AUTH at a file of the test's own.
func tempAuth(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cameo", "auth.json")
	t.Setenv("CAMEO_AUTH", path)
	return path
}

// saveSignIn keeps in as the ChatGPT sign-in in the file CAMEO_AUTH names.
func saveSignIn(t *testing.T, in *signIn) *authStore {
	t.Helper()
	store, err := openAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.update(context.Background(), chatgptProvider, func(*signIn) (*signIn, error) { return in, nil }); err != nil {
		t.Fatal(err)
	}
	return store
}

func readAuthFile(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return out
}

// fakeAuth is auth.openai.com as Sign in with ChatGPT uses it. Like the
// real one it hands out a new refresh token with each renewal and turns
// down one used before.
type fakeAuth struct {
	url string

	mu        sync.Mutex
	valid     string // the refresh token taken now
	refreshes int
	exchanges int
	codes     []url.Values // the code exchanges asked for
	renewals  []url.Values // the renewals asked for
	revoked   []url.Values
	// What the ID token says, when it should not be what is expected.
	nonce, issuer, audience string
	idExpired               bool
	noID                    bool
	// scope and expiresIn are what a token answer grants.
	scope     string
	expiresIn int
	// challenge is the PKCE challenge a code exchange's verifier must fit.
	challenge string
	// refuse is the OAuth code every renewal is refused with, and
	// refuseStatus the status of a refusal that names no code.
	refuse       string
	refuseStatus int
	// noRevocation leaves the revocation endpoint out of the discovery.
	noRevocation bool
	// delay is how long a renewal takes, to let others pile up behind it.
	delay time.Duration
}

// newFakeAuth starts it and points cameo at it.
func newFakeAuth(t *testing.T) *fakeAuth {
	t.Helper()
	f := &fakeAuth{valid: "refresh-0", scope: allScopes, expiresIn: 3600}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.url = srv.URL
	oldI, oldA, oldT := chatgptIssuer, chatgptAuthorizeURL, chatgptTokenURL
	chatgptIssuer, chatgptAuthorizeURL, chatgptTokenURL = srv.URL, srv.URL+"/api/accounts/authorize", srv.URL+"/api/accounts/oauth/token"
	t.Cleanup(func() { chatgptIssuer, chatgptAuthorizeURL, chatgptTokenURL = oldI, oldA, oldT })
	return f
}

func (f *fakeAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	r.ParseForm()
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		doc := map[string]string{"issuer": f.url}
		if !f.noRevocation {
			doc["revocation_endpoint"] = f.url + "/oauth/revoke"
		}
		json.NewEncoder(w).Encode(doc)
	case "/oauth/revoke":
		f.mu.Lock()
		f.revoked = append(f.revoked, r.PostForm)
		f.mu.Unlock()
	case "/api/accounts/oauth/token":
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
			return
		}
		if r.PostForm.Get("grant_type") == "refresh_token" {
			time.Sleep(f.delay)
			f.renew(w, r.PostForm)
		} else {
			f.exchange(w, r.PostForm)
		}
	default:
		http.NotFound(w, r)
	}
}

func refuse(w http.ResponseWriter, code, desc string) {
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

func (f *fakeAuth) exchange(w http.ResponseWriter, form url.Values) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exchanges++
	f.codes = append(f.codes, form)
	sum := sha256.Sum256([]byte(form.Get("code_verifier")))
	switch {
	case form.Get("grant_type") != "authorization_code" || form.Get("code") != "good-code":
		refuse(w, "invalid_grant", "the code is not good")
	case form.Get("client_id") != testClient:
		refuse(w, "invalid_client", "not the client that was issued")
	case f.challenge != "" && base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge:
		refuse(w, "invalid_grant", "the verifier does not fit the challenge")
	default:
		f.issue(w, form)
	}
}

func (f *fakeAuth) renew(w http.ResponseWriter, form url.Values) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewals = append(f.renewals, form)
	switch {
	case f.refuseStatus != 0:
		w.WriteHeader(f.refuseStatus)
		io.WriteString(w, "<html>not now</html>")
	case f.refuse != "":
		// as an object, the other way the endpoint has of saying it
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": f.refuse, "message": "no"}})
	case form.Get("client_id") != testClient:
		refuse(w, "invalid_client", "not the client that was issued")
	case form.Get("refresh_token") != f.valid:
		refuse(w, "refresh_token_reused", "that refresh token was used before")
	default:
		f.refreshes++
		f.issue(w, form)
	}
}

// issue answers with the next tokens. The caller holds the lock.
func (f *fakeAuth) issue(w http.ResponseWriter, form url.Values) {
	n := f.refreshes + f.exchanges
	f.valid = fmt.Sprintf("refresh-%d", n)
	out := map[string]any{
		"access_token":  fmt.Sprintf("access-%d", n),
		"refresh_token": f.valid,
		"token_type":    "Bearer",
		"expires_in":    f.expiresIn,
		"scope":         f.scope,
	}
	if !f.noID {
		claims := map[string]any{
			"iss": f.url, "aud": form.Get("client_id"), "nonce": f.nonce,
			"sub": "user-1", "email": "user@example.com", "exp": time1h().Unix(),
			chatgptClaims: map[string]any{"chatgpt_plan_type": "plus"},
		}
		if f.issuer != "" {
			claims["iss"] = f.issuer
		}
		if f.audience != "" {
			claims["aud"] = []string{f.audience}
		}
		if f.idExpired {
			claims["exp"] = time.Now().Add(-time.Hour).Unix()
		}
		out["id_token"] = jwt(claims)
	}
	json.NewEncoder(w).Encode(out)
}

func (f *fakeAuth) counts() (refreshes, exchanges, asked int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshes, f.exchanges, len(f.renewals)
}

// fakeAPI points cameo at a test server standing in for api.openai.com.
func fakeAPI(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	old := chatgptAPI
	chatgptAPI = srv.URL + "/v1"
	t.Cleanup(func() { chatgptAPI = old })
}

// newLocalListener holds a port on this machine and returns its address.
func newLocalListener(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}
