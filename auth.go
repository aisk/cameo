package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// signIn is what cameo keeps of one signed-in subscription.
type signIn struct {
	// ClientID is the client OpenAI registered for cameo at this sign-in.
	// Renewals and the sign-out go with it.
	ClientID     string    `json:"client_id"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	IDToken      string    `json:"id_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scopes       []string  `json:"scopes"`
	Subject      string    `json:"subject"`
	Email        string    `json:"email"`
	Plan         string    `json:"plan"`
	LastRefresh  time.Time `json:"last_refresh"`
}

// lockWait bounds how long a writer waits for another one to be done, and
// lockPoll is how often it looks.
var (
	lockWait = time.Minute
	lockPoll = 25 * time.Millisecond
)

// authStore is the file of sign-ins, one entry per built-in provider. Every
// cameo process shares it, so a change is made under a lock file next to
// it, read again from disk first, and written by renaming a whole new file
// into place: a reader never sees half of one.
type authStore struct {
	path string
	// sem is the lock inside this process. A channel, so that waiting for
	// it ends with the context like waiting for the file lock does.
	sem chan struct{}
}

func authPath() (string, error) {
	if p := os.Getenv("CAMEO_AUTH"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cameo", "auth.json"), nil
}

func openAuthStore() (*authStore, error) {
	path, err := authPath()
	if err != nil {
		return nil, err
	}
	return newAuthStore(path), nil
}

func newAuthStore(path string) *authStore {
	return &authStore{path: path, sem: make(chan struct{}, 1)}
}

func (s *authStore) read() (map[string]json.RawMessage, error) {
	entries := map[string]json.RawMessage{}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && len(bytes.TrimSpace(raw)) == 0) {
		return entries, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	return entries, nil
}

// load returns the sign-in kept for a provider, or nil when there is none.
func (s *authStore) load(name string) (*signIn, error) {
	entries, err := s.read()
	if err != nil {
		return nil, err
	}
	return entry(s.path, entries, name)
}

func entry(path string, entries map[string]json.RawMessage, name string) (*signIn, error) {
	raw, ok := entries[name]
	if !ok {
		return nil, nil
	}
	var in signIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("parse %s: %s: %w", path, name, err)
	}
	return &in, nil
}

// update changes the sign-in of one provider. change is given what the
// file holds once the lock is taken, which may be newer than what the
// caller last read, and answers what to keep: the same sign-in to leave
// the file alone, or nil to remove the entry.
func (s *authStore) update(ctx context.Context, name string, change func(cur *signIn) (*signIn, error)) (*signIn, error) {
	ctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting to update %s: %w", s.path, ctx.Err())
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, err
	}
	unlock, err := lockFile(ctx, s.path+".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()

	entries, err := s.read()
	if err != nil {
		return nil, err
	}
	cur, err := entry(s.path, entries, name)
	if err != nil {
		return nil, err
	}
	next, err := change(cur)
	if err != nil || next == cur {
		return next, err
	}
	if next == nil {
		delete(entries, name)
	} else if entries[name], err = json.Marshal(next); err != nil {
		return nil, err
	}
	return next, s.write(entries)
}

func (s *authStore) write(entries map[string]json.RawMessage) error {
	out, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	// CreateTemp makes the file readable by its owner only.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".auth-*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(out, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = replaceFile(tmp.Name(), s.path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// replaceFile renames from over to. Windows refuses that for the moment
// another process has to open, which a reader does without the lock, so it
// is tried a few times.
func replaceFile(from, to string) error {
	var err error
	for range 10 {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(lockPoll)
	}
	return err
}

// lockFile takes the lock every cameo process changing the file next to it
// holds meanwhile. The lock belongs to the open file, so the system gives
// it back when its holder exits, however that happens. The file itself is
// never removed: a process still holding a removed one would no longer
// keep anyone out.
func lockFile(ctx context.Context, path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		ok, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if ok {
			return func() { f.Close() }, nil
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, ctx.Err())
		case <-time.After(lockPoll):
		}
	}
}
