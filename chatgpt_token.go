package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

// signInStatus is the status Claude Code is answered with when the ChatGPT
// sign-in is missing or no longer good. Not 401, which Claude Code takes
// as its own Anthropic login having gone bad and answers by sending the
// user to /login, and not 403, which it may read as that login revoked.
// Not a 5xx or 429 either, which it retries for minutes. A 400 is shown
// once, as it is, with the message saying what to run.
const signInStatus = http.StatusBadRequest

const chatgptLoginHint = "run `cameo provider login chatgpt`"

// statusError is a failure cameo answers the client with under a status of
// its own choosing, instead of the 502 any other failure to reach the
// upstream gets.
type statusError struct {
	status int
	msg    string
}

func (e *statusError) Error() string { return e.msg }

var (
	errSignedOut  = &statusError{signInStatus, "chatgpt is not signed in, " + chatgptLoginHint}
	errSignInGone = &statusError{signInStatus, "the ChatGPT sign-in has expired or was revoked, " + chatgptLoginHint + " to sign in again"}
	errClientGone = &statusError{signInStatus, "OpenAI no longer knows the client cameo registered at sign-in, " + chatgptLoginHint + " to sign in again"}
)

// refreshBefore is how long before it expires an access token is renewed,
// so that no request starts with one about to.
const refreshBefore = 3 * time.Minute

// refreshGrace is how old a renewal must be before a 401 leads to another.
// Several requests sent with the same token are turned away together, and
// one renewal, here or in another cameo process, answers for all of them.
var refreshGrace = time.Minute

// chatgptAccount hands out the tokens of the ChatGPT sign-in, renewed when
// they need to be.
type chatgptAccount struct {
	store *authStore

	mu sync.Mutex
	// dead is a refresh token OpenAI turned down for good, and why. It is
	// not offered again.
	dead    string
	deadErr error
}

// token returns a sign-in whose access token is good for a while yet. The
// file is read every time, so a renewal, a new sign-in or a sign-out made
// by another process is seen at once.
func (a *chatgptAccount) token(ctx context.Context) (*signIn, error) {
	cur, err := a.store.load(chatgptProvider)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, errSignedOut
	}
	if !cur.expiring() {
		return cur, nil
	}
	return a.refresh(ctx, false)
}

// refresh renews the tokens under the lock of the sign-in file, unless
// what the file holds by then needs no renewal: another process has done
// it. force is for a token OpenAI turned away, whatever its expiry.
func (a *chatgptAccount) refresh(ctx context.Context, force bool) (*signIn, error) {
	return a.store.update(ctx, chatgptProvider, func(cur *signIn) (*signIn, error) {
		switch {
		case cur == nil:
			return nil, errSignedOut
		case force && time.Since(cur.LastRefresh) < refreshGrace:
			return cur, nil
		case !force && !cur.expiring():
			return cur, nil
		}
		if err := a.deadFor(cur.RefreshToken); err != nil {
			return nil, err
		}
		next, err := refreshTokens(ctx, cur)
		var se *statusError
		switch {
		case errors.As(err, &se):
			a.setDead(cur.RefreshToken, err)
		case err != nil && !force && time.Until(cur.ExpiresAt) > 0:
			// A hiccup, and the token in hand still does.
			return cur, nil
		}
		return next, err
	})
}

func (a *chatgptAccount) deadFor(refreshToken string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dead != "" && a.dead == refreshToken {
		return a.deadErr
	}
	return nil
}

func (a *chatgptAccount) setDead(refreshToken string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dead, a.deadErr = refreshToken, err
}

// expiring says whether the access token has less than refreshBefore left.
func (in *signIn) expiring() bool {
	return time.Until(in.ExpiresAt) < refreshBefore
}
