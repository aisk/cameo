// Command cameo runs Claude Code behind a local proxy that lets subagents be
// served by third-party providers.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "cameo:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(args []string) (int, error) {
	path, err := configPath()
	if err != nil {
		return 0, err
	}
	cfg, err := loadConfig(path)
	if err != nil {
		return 0, err
	}

	logger, closeLog, err := openLog()
	if err != nil {
		return 0, err
	}
	defer closeLog()

	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return 0, err
	}
	proxy, err := newProxy(cfg, hex.EncodeToString(token), logger)
	if err != nil {
		return 0, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	go func() {
		srv := &http.Server{Handler: proxy, ErrorLog: logger}
		if err := srv.Serve(ln); err != nil && !errors.Is(err, net.ErrClosed) {
			logger.Printf("proxy: %v", err)
		}
	}()
	baseURL := "http://" + ln.Addr().String() + proxy.prefix
	logger.Printf("proxy listening on %s, upstream %s", ln.Addr(), cfg.Upstream)

	agents, err := agentsJSON(cfg)
	if err != nil {
		return 0, err
	}
	cmd := exec.Command("claude", append([]string{"--agents", agents}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "ANTHROPIC_BASE_URL="+baseURL)

	// The terminal delivers SIGINT to claude directly, so only swallow it
	// here. Signals aimed at cameo alone are passed on.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	go func() {
		for sig := range sigs {
			if sig != syscall.SIGINT {
				cmd.Process.Signal(sig)
			}
		}
	}()

	err = cmd.Wait()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 0, err
}

// agentsJSON builds the value of claude's --agents flag.
func agentsJSON(cfg *Config) (string, error) {
	type agentDef struct {
		Description string   `json:"description"`
		Prompt      string   `json:"prompt"`
		Tools       []string `json:"tools,omitempty"`
		Model       string   `json:"model"`
	}
	defs := make(map[string]agentDef, len(cfg.Agents))
	for name, a := range cfg.Agents {
		model := modelPrefix + name
		if a.Context1M {
			model += context1M
		}
		defs[name] = agentDef{
			Description: a.Description,
			Prompt:      a.Prompt,
			Tools:       a.Tools,
			Model:       model,
		}
	}
	out, err := json.Marshal(defs)
	return string(out), err
}

// openLog returns a logger writing to the file named by CAMEO_LOG. Logging
// is off by default, since stderr belongs to claude's UI.
func openLog() (*log.Logger, func(), error) {
	path := os.Getenv("CAMEO_LOG")
	if path == "" {
		return log.New(io.Discard, "", 0), func() {}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, err
	}
	return log.New(f, "", log.LstdFlags), func() { f.Close() }, nil
}
