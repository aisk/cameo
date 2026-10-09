package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aisk/cameo/llmconv"
)

const providerUsage = `Usage:
  cameo provider login <name>    sign in to a built-in provider (chatgpt)
  cameo provider logout <name>   remove the saved sign-in and revoke it
  cameo provider list            show the providers and their sign-ins
  cameo provider models <name>   show the models the account can use
`

// chatgptSharedUsage is said once somebody has signed in.
const chatgptSharedUsage = "Usage counts against the plan's limit, which is shared with ChatGPT and the other apps it is used in: " + chatgptUsageURL

// signInTimeout is how long a sign-in waits for the browser.
var signInTimeout = 10 * time.Minute

// providerCommand runs `cameo provider ...`, the one command cameo does
// not hand to claude.
func providerCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	sub, name := "", ""
	switch len(args) {
	case 1:
		sub = args[0]
	case 2:
		sub, name = args[0], args[1]
	}
	if (sub == "list") != (name == "") || !slices.Contains([]string{"login", "logout", "list", "models"}, sub) {
		fmt.Fprint(stderr, providerUsage)
		return 2, nil
	}
	store, err := openAuthStore()
	if err != nil {
		return 0, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	switch sub {
	case "login":
		err = providerLogin(ctx, store, name, stdin, stdout)
	case "logout":
		err = providerLogout(ctx, store, name, stdout)
	case "list":
		err = providerList(store, stdout)
	case "models":
		err = providerModels(ctx, store, name, stdout)
	}
	return 0, err
}

// needsSignIn says whether name is a provider there is a sign-in for, and
// if not, why not.
func needsSignIn(name string) error {
	switch {
	case slices.Contains(builtinProviders, name):
		return nil
	case slices.Contains(reservedProviders, name):
		return fmt.Errorf("provider %q is not supported yet", name)
	}
	// Whatever is wrong with the config is not this command's to report.
	if cfg := readConfig(); cfg != nil && cfg.Providers[name] != nil {
		return fmt.Errorf("provider %q uses an API key from the config and needs no sign-in", name)
	}
	return fmt.Errorf("unknown provider %q, the built-in providers are: %s", name, strings.Join(builtinProviders, ", "))
}

// readConfig is the config if there is a readable one, else nil.
func readConfig() *Config {
	path, err := configPath()
	if err != nil {
		return nil
	}
	cfg, err := loadConfig(path)
	if err != nil {
		return nil
	}
	return cfg
}

func providerLogin(ctx context.Context, store *authStore, name string, stdin io.Reader, out io.Writer) error {
	if err := needsSignIn(name); err != nil {
		return err
	}
	host, err := hostID(filepath.Dir(store.path))
	if err != nil {
		return err
	}
	ln, err := listenCallback()
	if err != nil {
		return err
	}
	// The redirect names 127.0.0.1, whose port OpenAI lets vary.
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	flow := newLoginFlow("http://127.0.0.1:"+port+"/auth/callback", host)
	srv := &http.Server{Handler: flow, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer func() {
		// Lets the browser have its page before the port is given up.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	fmt.Fprintln(out, "Finish signing in in your browser. If it did not open, go to:")
	fmt.Fprintln(out, flow.authorizeURL())
	fmt.Fprintln(out, "If the page the browser ends on does not load (cameo on another machine), paste its whole address here and press Enter:")
	openBrowser(flow.authorizeURL())
	go readPasted(ctx, flow, stdin, out)

	ctx, cancel := context.WithTimeout(ctx, signInTimeout)
	defer cancel()
	select {
	case res := <-flow.done:
		if res.err != nil {
			return fmt.Errorf("sign-in didn't finish: %w", res.err)
		}
		if _, err := store.update(ctx, name, func(*signIn) (*signIn, error) { return res.in, nil }); err != nil {
			return err
		}
		fmt.Fprintf(out, "Signed in to %s as %s.\n", name, res.in.who())
		fmt.Fprintln(out, chatgptSharedUsage)
		return nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errors.New("sign-in timed out")
		}
		return errors.New("sign-in canceled")
	}
}

// readPasted feeds the flow the addresses typed at the terminal, until one
// of them is the one.
func readPasted(ctx context.Context, flow *loginFlow, stdin io.Reader, out io.Writer) {
	lines := bufio.NewScanner(stdin)
	lines.Buffer(nil, 1<<20)
	for lines.Scan() {
		if strings.TrimSpace(lines.Text()) == "" {
			continue
		}
		err := flow.pasted(ctx, lines.Text())
		if err == nil {
			return
		}
		fmt.Fprintln(out, err)
	}
}

// openBrowser opens url in the user's browser, if it can. A variable, so
// tests can stand in for the browser.
var openBrowser = func(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}

// revokeTimeout is how long signing out waits for OpenAI.
var revokeTimeout = 10 * time.Second

// providerLogout removes the sign-in and has OpenAI revoke its refresh
// token. The sign-in is gone from this machine whether or not OpenAI can
// be reached.
func providerLogout(ctx context.Context, store *authStore, name string, out io.Writer) error {
	if err := needsSignIn(name); err != nil {
		return err
	}
	var was *signIn
	_, err := store.update(ctx, name, func(cur *signIn) (*signIn, error) {
		was = cur
		return nil, nil
	})
	if err != nil {
		return err
	}
	if was == nil {
		fmt.Fprintf(out, "%s was not signed in.\n", name)
		return nil
	}
	fmt.Fprintf(out, "Signed out of %s.\n", name)
	ctx, cancel := context.WithTimeout(ctx, revokeTimeout)
	defer cancel()
	if err := revoke(ctx, was); err != nil {
		fmt.Fprintf(out, "The sign-in is removed from this machine, but OpenAI could not be asked to revoke it: %v\n", err)
	}
	return nil
}

// providerList shows the built-in providers and those of the config. A
// config that is missing leaves the built-in ones, and one whose agents
// wait for a sign-in is read all the same.
func providerList(store *authStore, out io.Writer) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	cfg := &Config{}
	if _, err := os.Stat(path); err == nil {
		if cfg, err = loadConfig(path); err != nil {
			return err
		}
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tAPI\tDETAILS")
	for _, name := range builtinProviders {
		in, err := store.load(name)
		if err != nil {
			return err
		}
		status := "not signed in"
		if in != nil {
			status = "signed in as " + in.who()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", name, llmconv.Responses, status)
	}
	for _, name := range sortedKeys(cfg.Providers) {
		fmt.Fprintf(w, "%s\t%s\t%s\n", name, cfg.Providers[name].API, cfg.Providers[name].URL)
	}
	return w.Flush()
}

func providerModels(ctx context.Context, store *authStore, name string, out io.Writer) error {
	switch {
	case slices.Contains(builtinProviders, name):
	case slices.Contains(reservedProviders, name):
		return fmt.Errorf("provider %q is not supported yet", name)
	default:
		return fmt.Errorf("listing models is not supported for provider %q, only for: %s", name, strings.Join(builtinProviders, ", "))
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	models, err := (&chatgptAccount{store: store}).models(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "MODEL\tNAME\tREASONING")
	for _, m := range models {
		fmt.Fprintf(w, "%s\t%s\t%s\n", m.Slug, m.DisplayName, strings.Join(m.efforts(), ", "))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out, "The sign-in may also run newer models that this list leaves out.")
	return nil
}
