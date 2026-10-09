package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAuthPath(t *testing.T) {
	t.Setenv("CAMEO_AUTH", "/somewhere/else.json")
	if got, _ := authPath(); got != "/somewhere/else.json" {
		t.Errorf("with CAMEO_AUTH: %q", got)
	}
	t.Setenv("CAMEO_AUTH", "")
	t.Setenv("CAMEO_CONFIG", "")
	cfg, _ := configPath()
	if got, _ := authPath(); got != filepath.Join(filepath.Dir(cfg), "auth.json") {
		t.Errorf("default %q is not next to %q", got, cfg)
	}
}

func TestAuthFile(t *testing.T) {
	path := tempAuth(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// An entry of a provider this cameo knows nothing of is kept as it is.
	if err := os.WriteFile(path, []byte(`{"later": {"token": "t", "more": [1, 2]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := saveSignIn(t, testSignIn(time1h()))

	entries := readAuthFile(t, path)
	var keys []string
	for k := range entries[chatgptProvider] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got := strings.Join(keys, " "); got != "access_token client_id email expires_at id_token last_refresh plan refresh_token scopes subject" {
		t.Errorf("fields = %s", got)
	}
	if got, _ := json.Marshal(entries["later"]); string(got) != `{"more":[1,2],"token":"t"}` {
		t.Errorf("the other entry became %s", got)
	}
	in, err := store.load(chatgptProvider)
	if err != nil || in.Email != "user@example.com" || in.RefreshToken != "refresh-0" || in.LastRefresh.IsZero() {
		t.Errorf("loaded %+v, %v", in, err)
	}
	if in, err := store.load("nobody"); in != nil || err != nil {
		t.Errorf("loaded %+v, %v", in, err)
	}

	// Removing leaves the others.
	if _, err := store.update(context.Background(), chatgptProvider, func(*signIn) (*signIn, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if entries := readAuthFile(t, path); len(entries) != 1 || entries["later"] == nil {
		t.Errorf("after removing: %v", entries)
	}
}

func TestAuthFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no permission bits")
	}
	path := tempAuth(t)
	saveSignIn(t, testSignIn(time1h()))
	saveSignIn(t, testSignIn(time1h())) // and once over an existing file

	for name, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700, path + ".lock": 0o600} {
		st, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != want {
			t.Errorf("%s: mode %o, want %o", name, got, want)
		}
	}
	files, _ := os.ReadDir(filepath.Dir(path))
	if len(files) != 2 {
		t.Errorf("left behind: %v", files)
	}
}

// A reader that takes no lock must never see a file half written.
func TestAuthFileIsReplacedWhole(t *testing.T) {
	path := tempAuth(t)
	store := saveSignIn(t, testSignIn(time1h()))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				in, err := newAuthStore(path).load(chatgptProvider)
				if err != nil || in == nil || !strings.HasPrefix(in.RefreshToken, "refresh-") {
					t.Errorf("read %+v, %v", in, err)
					return
				}
			}
		})
	}
	for i := range 200 {
		in := testSignIn(time1h())
		in.RefreshToken = "refresh-" + strings.Repeat("x", i)
		if _, err := store.update(context.Background(), chatgptProvider, func(*signIn) (*signIn, error) { return in, nil }); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestAuthFileKeptWhenChangeFails(t *testing.T) {
	path := tempAuth(t)
	store := saveSignIn(t, testSignIn(time1h()))
	before, _ := os.ReadFile(path)

	boom := errors.New("boom")
	if _, err := store.update(context.Background(), chatgptProvider, func(*signIn) (*signIn, error) { return nil, boom }); err != boom {
		t.Errorf("err = %v", err)
	}
	// The same sign-in back means nothing to write.
	st, _ := os.Stat(path)
	if _, err := store.update(context.Background(), chatgptProvider, func(cur *signIn) (*signIn, error) { return cur, nil }); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if st2, _ := os.Stat(path); string(before) != string(after) || !os.SameFile(st, st2) {
		t.Error("the file was written")
	}
}

func TestLockKeepsOthersOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json.lock")
	unlock, err := lockFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := lockFile(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a second lock: %v", err)
	}
	unlock()
	again, err := lockFile(context.Background(), path)
	if err != nil {
		t.Fatalf("after unlocking: %v", err)
	}
	again()
}

// A store waiting for the lock gives up with its context, and in the end
// by itself.
func TestUpdateGivesUpWaiting(t *testing.T) {
	path := tempAuth(t)
	store := saveSignIn(t, testSignIn(time1h()))
	unlock, err := lockFile(context.Background(), path+".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	old := lockWait
	lockWait = 100 * time.Millisecond
	defer func() { lockWait = old }()
	_, err = store.update(context.Background(), chatgptProvider, func(cur *signIn) (*signIn, error) {
		t.Error("changed without the lock")
		return cur, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
}
