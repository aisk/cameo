package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTokenIsUsedWhileGood(t *testing.T) {
	auth := newFakeAuth(t)
	tempAuth(t)
	account := &chatgptAccount{store: saveSignIn(t, testSignIn(time.Now().Add(refreshBefore+time.Minute)))}
	got, err := account.token(context.Background())
	if err != nil || got.AccessToken != "access-0" {
		t.Errorf("%+v, %v", got, err)
	}
	if _, _, asked := auth.counts(); asked != 0 {
		t.Errorf("%d renewals", asked)
	}
}

func TestSignedOut(t *testing.T) {
	tempAuth(t)
	store, _ := openAuthStore()
	account := &chatgptAccount{store: store}
	for _, err := range []error{
		func() error { _, err := account.token(context.Background()); return err }(),
		func() error { _, err := account.refresh(context.Background(), true); return err }(),
	} {
		if err != errSignedOut {
			t.Errorf("err = %v", err)
		}
	}
}

func TestRefreshNearExpiry(t *testing.T) {
	auth := newFakeAuth(t)
	path := tempAuth(t)
	old := testSignIn(time.Now().Add(refreshBefore - time.Minute))
	old.Plan = "free"
	account := &chatgptAccount{store: saveSignIn(t, old)}

	got, err := account.token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "access-1" || got.RefreshToken != "refresh-1" || got.ClientID != testClient {
		t.Errorf("tokens = %+v", got)
	}
	// Who it is stays, and the new ID token says what plan it is on now.
	if got.Email != "user@example.com" || got.Subject != "user-1" || got.Plan != "plus" || got.IDToken == "id-0" {
		t.Errorf("sign-in = %+v", got)
	}
	if left := time.Until(got.ExpiresAt); left < 59*time.Minute || time.Since(got.LastRefresh) > time.Minute {
		t.Errorf("expires in %v, renewed %v", left, got.LastRefresh)
	}
	// The renewal goes with the client id OpenAI issued, for the API.
	want := map[string]string{"grant_type": "refresh_token", "client_id": testClient, "refresh_token": "refresh-0", "resource": "https://api.openai.com/v1"}
	form := auth.renewals[0]
	for k, v := range want {
		if form.Get(k) != v {
			t.Errorf("%s = %q", k, form.Get(k))
		}
	}
	if len(form) != len(want) {
		t.Errorf("asked %v", form)
	}
	saved := readAuthFile(t, path)[chatgptProvider]
	if saved["access_token"] != "access-1" || saved["refresh_token"] != "refresh-1" || saved["client_id"] != testClient {
		t.Errorf("saved %v", saved)
	}
	// And once renewed, it is good.
	if again, err := account.token(context.Background()); err != nil || again.AccessToken != "access-1" {
		t.Errorf("again: %+v, %v", again, err)
	}
	if refreshes, _, _ := auth.counts(); refreshes != 1 {
		t.Errorf("%d renewals", refreshes)
	}
}

// Requests pile up on an expiring token in several goroutines of two cameo
// processes, each process with a store of its own on the one file. The
// token endpoint turns down a refresh token used twice, so anything but
// one renewal leaves somebody without a sign-in.
func TestRefreshUnderContention(t *testing.T) {
	for name, renew := range map[string]func(*chatgptAccount) (*signIn, error){
		"near expiry": func(a *chatgptAccount) (*signIn, error) { return a.token(context.Background()) },
		"after a 401": func(a *chatgptAccount) (*signIn, error) { return a.refresh(context.Background(), true) },
	} {
		t.Run(name, func(t *testing.T) {
			auth := newFakeAuth(t)
			auth.delay = 50 * time.Millisecond
			path := tempAuth(t)
			// Expired already, so a renewal that fails is not papered over
			// by the token in hand.
			saveSignIn(t, testSignIn(time.Now().Add(-time.Minute)))
			processes := []*chatgptAccount{{store: newAuthStore(path)}, {store: newAuthStore(path)}}

			const each = 8
			tokens := make(chan string, each*len(processes))
			var wg sync.WaitGroup
			for _, account := range processes {
				for range each {
					wg.Go(func() {
						in, err := renew(account)
						if err != nil {
							t.Error(err)
							return
						}
						tokens <- in.AccessToken
					})
				}
			}
			wg.Wait()
			close(tokens)

			refreshes, _, asked := auth.counts()
			if refreshes != 1 || asked != 1 {
				t.Errorf("%d renewals of %d asked for", refreshes, asked)
			}
			n := 0
			for tok := range tokens {
				n++
				if tok != "access-1" {
					t.Errorf("someone holds %q, not the renewed token", tok)
				}
			}
			if n != each*len(processes) {
				t.Errorf("%d of %d got a token", n, each*len(processes))
			}
			if saved := readAuthFile(t, path)[chatgptProvider]; saved["refresh_token"] != "refresh-1" {
				t.Errorf("saved %v", saved)
			}
		})
	}
}

// A 401 renews the tokens whatever their expiry says, but not when they
// were renewed a moment ago: that request was sent with the old ones.
func TestForcedRefresh(t *testing.T) {
	auth := newFakeAuth(t)
	tempAuth(t)
	account := &chatgptAccount{store: saveSignIn(t, testSignIn(time1h()))}

	got, err := account.refresh(context.Background(), true)
	if err != nil || got.AccessToken != "access-1" {
		t.Fatalf("%+v, %v", got, err)
	}
	again, err := account.refresh(context.Background(), true)
	if err != nil || again.AccessToken != "access-1" {
		t.Errorf("%+v, %v", again, err)
	}
	if refreshes, _, _ := auth.counts(); refreshes != 1 {
		t.Errorf("%d renewals", refreshes)
	}
}

func TestSignInGone(t *testing.T) {
	codes := map[string]error{"invalid_client": errClientGone}
	for _, code := range deadRefresh {
		codes[code] = errSignInGone
	}
	if len(codes) != 7 {
		t.Fatalf("codes = %v", codes)
	}
	for code, want := range codes {
		auth := newFakeAuth(t)
		auth.refuse = code
		path := tempAuth(t)
		// Even with a minute left on the token, the sign-in is over.
		account := &chatgptAccount{store: saveSignIn(t, testSignIn(time.Now().Add(time.Minute)))}

		_, err := account.token(context.Background())
		var se *statusError
		if err != want || !errors.As(err, &se) || se.status == http.StatusUnauthorized {
			t.Errorf("%s: err = %v", code, err)
		}
		// The dead refresh token is not offered again.
		if _, err := account.token(context.Background()); err != want {
			t.Errorf("%s: second err = %v", code, err)
		}
		if _, _, asked := auth.counts(); asked != 1 {
			t.Errorf("%s: asked %d times", code, asked)
		}
		if after := readAuthFile(t, path)[chatgptProvider]; after["refresh_token"] != "refresh-0" {
			t.Errorf("%s: the file changed: %v", code, after)
		}

		// Signing in again brings a refresh token that is tried.
		auth.mu.Lock()
		auth.refuse, auth.valid = "", "refresh-9"
		auth.mu.Unlock()
		fresh := testSignIn(time.Now())
		fresh.RefreshToken = "refresh-9"
		saveSignIn(t, fresh)
		if got, err := account.token(context.Background()); err != nil || got.RefreshToken == "refresh-9" {
			t.Errorf("%s: after signing in again: %+v, %v", code, got, err)
		}
	}
}

// A renewal that fails for no reason of the sign-in's is not its end.
func TestRefreshHiccup(t *testing.T) {
	for name, setup := range map[string]func(*fakeAuth){
		"a proxy in the way": func(f *fakeAuth) { f.refuseStatus = http.StatusBadGateway },
		"another refusal":    func(f *fakeAuth) { f.refuse = "temporarily_unavailable" },
	} {
		t.Run(name, func(t *testing.T) {
			auth := newFakeAuth(t)
			setup(auth)
			tempAuth(t)
			account := &chatgptAccount{store: saveSignIn(t, testSignIn(time.Now().Add(time.Minute)))}

			// The token in hand still does for the minute it has.
			got, err := account.token(context.Background())
			if err != nil || got.AccessToken != "access-0" {
				t.Errorf("%+v, %v", got, err)
			}
			// Not when OpenAI turned that very token away.
			if _, err := account.refresh(context.Background(), true); err == nil {
				t.Error("a forced renewal that failed went unremarked")
			}
			// And not once it has expired, though it is no statusError: the
			// next request tries again.
			account = &chatgptAccount{store: saveSignIn(t, testSignIn(time.Now().Add(-time.Minute)))}
			_, err = account.token(context.Background())
			var se *statusError
			if err == nil || errors.As(err, &se) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

// A request given up while its renewal is under way does not take the
// renewal with it: the refresh token is spent either way.
func TestRefreshOutlivesItsRequest(t *testing.T) {
	auth := newFakeAuth(t)
	auth.delay = 100 * time.Millisecond
	path := tempAuth(t)
	account := &chatgptAccount{store: saveSignIn(t, testSignIn(time.Now()))}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if got, err := account.token(ctx); err != nil || got.AccessToken != "access-1" {
		t.Errorf("%+v, %v", got, err)
	}
	if saved := readAuthFile(t, path)[chatgptProvider]; saved["refresh_token"] != "refresh-1" {
		t.Errorf("saved %v", saved)
	}
}

// A renewal that no longer grants the use of the plan through the API is
// not kept.
func TestRefreshNotEligible(t *testing.T) {
	auth := newFakeAuth(t)
	auth.scope = "openid profile email offline_access"
	path := tempAuth(t)
	account := &chatgptAccount{store: saveSignIn(t, testSignIn(time.Now().Add(-time.Minute)))}
	if _, err := account.token(context.Background()); err == nil || !strings.Contains(err.Error(), "may not be eligible") {
		t.Errorf("err = %v", err)
	}
	if saved := readAuthFile(t, path)[chatgptProvider]; saved["access_token"] != "access-0" {
		t.Errorf("saved %v", saved)
	}
}
