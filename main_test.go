package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestMain keeps every test away from OpenAI, the user's own files, port
// 1455 and a browser, also the tests that forget to.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cameo-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", dir)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	os.Setenv("CAMEO_AUTH", filepath.Join(dir, "auth.json"))
	os.Unsetenv("CAMEO_CONFIG")
	chatgptIssuer = "http://127.0.0.1:1"
	chatgptAuthorizeURL = "http://127.0.0.1:1/api/accounts/authorize"
	chatgptTokenURL = "http://127.0.0.1:1/api/accounts/oauth/token"
	chatgptAPI = "http://127.0.0.1:1/v1"
	chatgptCallbackAddr = "127.0.0.1:0"
	openBrowser = func(string) {}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fakeClaude puts a claude on PATH that writes its arguments to a file,
// one per line, and returns the file's path.
func fakeClaude(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell script on PATH")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + out + "'\nexit 7\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return out
}

func TestProviderIsHandledByCameo(t *testing.T) {
	args := fakeClaude(t)
	t.Setenv("CAMEO_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	// Neither a config nor claude is needed, and the usage error is cameo's.
	code, err := run([]string{"provider"})
	if code != 2 || err != nil {
		t.Errorf("code %d, err %v", code, err)
	}
	if _, err := os.Stat(args); err == nil {
		t.Error("claude was run")
	}
}

func TestOtherArgumentsGoToClaude(t *testing.T) {
	for _, argv := range [][]string{
		{"-p", "provider", "login"},
		{"providers"},
		{"--provider", "x"},
		{},
	} {
		args := fakeClaude(t)
		t.Setenv("CAMEO_CONFIG", writeFile(t, testProvider+agent("third")))
		code, err := run(argv)
		if code != 7 || err != nil {
			t.Fatalf("%q: code %d, err %v", argv, code, err)
		}
		raw, err := os.ReadFile(args)
		if err != nil {
			t.Fatalf("%q: claude was not run: %v", argv, err)
		}
		got := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
		if len(got) != len(argv)+2 || got[0] != "--agents" || !strings.Contains(got[1], `"cameo-helper"`) {
			t.Fatalf("%q: claude got %q", argv, got)
		}
		for i, want := range argv {
			if got[i+2] != want {
				t.Errorf("%q: claude got %q", argv, got)
			}
		}
	}
}

func TestStartNeedsSignIn(t *testing.T) {
	args := fakeClaude(t)
	t.Setenv("CAMEO_AUTH", filepath.Join(t.TempDir(), "auth.json"))
	t.Setenv("CAMEO_CONFIG", writeFile(t, agent("chatgpt")))
	_, err := run(nil)
	if err == nil || !strings.Contains(err.Error(), "cameo provider login chatgpt") || !strings.Contains(err.Error(), "agents.helper") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(args); err == nil {
		t.Error("claude was run")
	}

	saveSignIn(t, testSignIn(time1h()))
	if code, err := run(nil); code != 7 || err != nil {
		t.Errorf("signed in: code %d, err %v", code, err)
	}
}
