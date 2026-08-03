package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"gonx/internal/config"
	"gonx/internal/server"
)

const version = "0.4.0"

func main() {
	confPath := flag.String("c", "gonx.conf", "path to configuration file")
	testOnly := flag.Bool("t", false, "test configuration and exit")
	showVersion := flag.Bool("v", false, "print version and exit")
	sigCmd := flag.String("s", "", "send signal to a running gonx: reload, reopen, stop, status, ui")
	flag.Parse()

	if *showVersion {
		fmt.Printf("gonx version: gonx/%s\n", version)
		return
	}
	if *sigCmd != "" {
		if err := sendSignal(*sigCmd, *confPath); err != nil {
			fmt.Fprintf(os.Stderr, "gonx: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// -t validates the config without producing runtime logs or a log dir
	if *testOnly {
		cfg, err := loadConfig(*confPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gonx: [emerg] %v\n", err)
			os.Exit(1)
		}
		for _, w := range cfg.Warnings {
			fmt.Fprintf(os.Stderr, "gonx: [warn] %s\n", w)
		}
		fmt.Printf("gonx: the configuration file %s syntax is ok\n", *confPath)
		fmt.Printf("gonx: configuration file %s test is successful\n", *confPath)
		return
	}

	// runtime logs go to <exe_dir>/log: success.log / fail.log / console.log
	opts, console, closeConsole := setupLogging()
	defer closeConsole()

	cfg, err := loadConfig(*confPath)
	if err != nil {
		fmt.Fprintf(console, "gonx: [emerg] %v\n", err)
		os.Exit(1)
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintf(console, "gonx: [warn] %s\n", w)
	}

	inst, err := server.NewWithOptions(cfg, opts)
	if err != nil {
		fmt.Fprintf(console, "gonx: [emerg] %v\n", err)
		os.Exit(1)
	}
	ru := &runner{confPath: *confPath, inst: inst, opts: opts, console: console, errs: make(chan error, 8), quit: make(chan struct{})}
	ru.watch(inst.Start())

	stopCtl, err := startControl(ru, *confPath, cfg.UI)
	if err != nil {
		fmt.Fprintf(console, "gonx: [warn] control channel unavailable (-s will not work): %v\n", err)
	} else {
		defer stopCtl()
	}

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-ru.errs:
		fmt.Fprintf(console, "gonx: [emerg] %v\n", err)
		os.Exit(1)
	case sig := <-sigc:
		fmt.Fprintf(console, "gonx: received %v, shutting down gracefully\n", sig)
	case <-ru.quit:
		fmt.Fprintln(console, "gonx: stop requested via control channel, shutting down gracefully")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ru.shutdown(ctx); err != nil {
		fmt.Fprintf(console, "gonx: shutdown: %v\n", err)
		os.Exit(1)
	}
}

// setupLogging prepares the <exe_dir>/log directory and returns server
// Options plus a console writer that tees stderr into log/console.log.
func setupLogging() (server.Options, io.Writer, func()) {
	logDir := "log"
	if exe, err := os.Executable(); err == nil {
		logDir = filepath.Join(filepath.Dir(exe), "log")
	}
	if err := os.MkdirAll(logDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "gonx: [warn] cannot create log dir %s: %v\n", logDir, err)
		return server.Options{}, os.Stderr, func() {}
	}
	f, err := os.OpenFile(filepath.Join(logDir, "console.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gonx: [warn] cannot open console.log: %v\n", err)
		return server.Options{LogDir: logDir}, os.Stderr, func() {}
	}
	console := io.MultiWriter(os.Stderr, f)
	return server.Options{LogDir: logDir, Console: console}, console, func() { f.Close() }
}

// runner owns the live server instance so reloads can swap it atomically.
type runner struct {
	mu       sync.Mutex
	confPath string
	inst     *server.Instance
	opts     server.Options
	console  io.Writer
	errs     chan error
	quit     chan struct{}
	stopOnce sync.Once
}

func (ru *runner) watch(c <-chan error) {
	go func() {
		if err, ok := <-c; ok {
			ru.errs <- err
		}
	}()
}

// reload builds a full new instance first, so a broken config never takes
// down the running one. The port handoff is not zero-downtime: old listeners
// must close before the new ones can bind.
func (ru *runner) reload() error {
	cfg, err := loadConfig(ru.confPath)
	if err != nil {
		return err
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintf(ru.console, "gonx: [warn] %s\n", w)
	}
	ni, err := server.NewWithOptions(cfg, ru.opts)
	if err != nil {
		return err
	}
	ru.mu.Lock()
	defer ru.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	ru.inst.Shutdown(ctx)
	cancel()
	ru.inst = ni
	ru.watch(ni.Start())
	fmt.Fprintln(ru.console, "gonx: configuration reloaded")
	return nil
}

func (ru *runner) shutdown(ctx context.Context) error {
	ru.mu.Lock()
	defer ru.mu.Unlock()
	return ru.inst.Shutdown(ctx)
}

func (ru *runner) requestStop() {
	ru.stopOnce.Do(func() { close(ru.quit) })
}

// startControl serves the admin console and reload/stop/status on a loopback
// port — inside the same gonx process as the proxy itself. The address and
// an access token live in <conf>.ctl, which "gonx -s <cmd> -c <conf>" reads.
// A "ui" config section pins the address/token and can auto-open the browser.
func startControl(ru *runner, confPath string, ui *config.UIConfig) (cleanup func(), err error) {
	listen := "127.0.0.1:0"
	if ui != nil && ui.Listen != "" {
		listen = ui.Listen
		if host, _, err := net.SplitHostPort(listen); err != nil || host == "" || host == "0.0.0.0" || host == "::" {
			fmt.Fprintf(ru.console, "gonx: [warn] ui.listen %q is not loopback-only; the console will be reachable from the network\n", listen)
		}
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	var token string
	if ui != nil && ui.Token != "" {
		token = ui.Token
	} else {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			ln.Close()
			return nil, err
		}
		token = hex.EncodeToString(buf)
	}
	ctlPath := confPath + ".ctl"
	if err := os.WriteFile(ctlPath, []byte(ln.Addr().String()+" "+token), 0600); err != nil {
		ln.Close()
		return nil, err
	}

	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !tokenOK(r, token) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			h(w, r)
		}
	}
	mux := http.NewServeMux()
	registerUI(mux, ru, token, auth)
	mux.HandleFunc("/reload", auth(func(w http.ResponseWriter, r *http.Request) {
		if err := ru.reload(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		io.WriteString(w, "reloaded\n")
	}))
	mux.HandleFunc("/reopen", auth(func(w http.ResponseWriter, r *http.Request) {
		ru.mu.Lock()
		err := ru.inst.Reopen()
		ru.mu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		io.WriteString(w, "logs reopened\n")
	}))
	mux.HandleFunc("/stop", auth(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "stopping\n")
		ru.requestStop()
	}))
	mux.HandleFunc("/status", auth(func(w http.ResponseWriter, r *http.Request) {
		ru.mu.Lock()
		addrs := make([]string, 0, len(ru.inst.Addrs))
		for _, a := range ru.inst.Addrs {
			addrs = append(addrs, a.Addr)
		}
		ru.mu.Unlock()
		fmt.Fprintf(w, "gonx/%s pid=%d listening=%s\n", version, os.Getpid(), strings.Join(addrs, ","))
	}))

	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	url := "http://" + ln.Addr().String() + "/ui?token=" + token
	fmt.Fprintf(ru.console, "gonx: 管理页面 %s\n", url)
	if ui != nil && ui.Open {
		openBrowser(url)
	}
	return func() {
		srv.Close()
		os.Remove(ctlPath)
	}, nil
}

func sendSignal(action, confPath string) error {
	var path string
	switch action {
	case "ui":
		return openUI(confPath)
	case "reload", "reopen", "status":
		path = "/" + action
	case "stop", "quit":
		path = "/stop"
	default:
		return fmt.Errorf("unknown signal %q (use reload, reopen, stop, status or ui)", action)
	}
	data, err := os.ReadFile(confPath + ".ctl")
	if err != nil {
		return fmt.Errorf("cannot read control file %s.ctl (is gonx running with -c %s?)", confPath, confPath)
	}
	parts := strings.Fields(string(data))
	if len(parts) != 2 {
		return errors.New("malformed control file")
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+parts[0]+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Gonx-Token", parts[1])
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	fmt.Print(string(body))
	return nil
}

func loadConfig(path string) (*config.Config, error) {
	return config.LoadFile(path)
}
