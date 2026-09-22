package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"crest/internal/config"
	"crest/internal/server"
)

const version = "0.7.0"

func main() {
	server.Version = version
	if addr := os.Getenv("CREST_PPROF"); addr != "" {
		go http.ListenAndServe(addr, nil) // net/http/pprof registers on DefaultServeMux
	}
	confPath := flag.String("c", "", "path to configuration file (default: the first of "+
		strings.Join(configNames, " / ")+" found in the working directory, then beside the executable)")
	testOnly := flag.Bool("t", false, "test configuration and exit")
	showVersion := flag.Bool("v", false, "print version and exit")
	sigCmd := flag.String("s", "", "send signal to a running crest: reload, reopen, stop, status, ui")
	flag.Parse()

	if *showVersion {
		fmt.Printf("crest version: crest/%s\n", version)
		return
	}
	// 双击启动时没有命令行参数，此处自动找配置；显式 -c 一律优先。
	// 这里先不因错误退出：-s / -t 是从终端运行的，报到 stderr 即可；而正常
	// 启动多半来自双击，窗口会随失败一同消失，得等日志就绪后报进 console.log。
	resolved, autoFound, confErr := resolveConfigPath(*confPath)

	if *sigCmd != "" {
		if confErr != nil {
			fmt.Fprintf(os.Stderr, "crest: %v\n", confErr)
			os.Exit(1)
		}
		if err := sendSignal(*sigCmd, resolved); err != nil {
			fmt.Fprintf(os.Stderr, "crest: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// -t validates the config without producing runtime logs or a log dir
	if *testOnly {
		if confErr != nil {
			fmt.Fprintf(os.Stderr, "crest: %v\n", confErr)
			os.Exit(1)
		}
		cfg, err := loadConfig(resolved)
		if err != nil {
			fmt.Fprintf(os.Stderr, "crest: [emerg] %v\n", err)
			os.Exit(1)
		}
		for _, w := range cfg.Warnings {
			fmt.Fprintf(os.Stderr, "crest: [warn] %s\n", w)
		}
		fmt.Printf("crest: the configuration file %s syntax is ok\n", resolved)
		fmt.Printf("crest: configuration file %s test is successful\n", resolved)
		return
	}

	// runtime logs go to <exe_dir>/log: success.log / fail.log / console.log
	opts, console, closeConsole := setupLogging()
	defer closeConsole()

	// 此刻 console 已同时写向 stderr 与 log/console.log：双击启动若因找不到
	// 配置而失败，窗口关掉之后仍能在 console.log 里看到找过哪些路径。
	if confErr != nil {
		fmt.Fprintf(console, "crest: [emerg] %v\n", confErr)
		os.Exit(1)
	}
	if autoFound {
		fmt.Fprintf(console, "crest: using configuration file %s\n", resolved)
	}
	cfg, err := loadConfig(resolved)
	if err != nil {
		fmt.Fprintf(console, "crest: [emerg] %v\n", err)
		os.Exit(1)
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintf(console, "crest: [warn] %s\n", w)
	}

	inst, err := server.NewWithOptions(cfg, opts)
	if err != nil {
		fmt.Fprintf(console, "crest: [emerg] %v\n", err)
		os.Exit(1)
	}
	ru := &runner{confPath: resolved, inst: inst, opts: opts, console: console, ui: cfg.UI, errs: make(chan error, 8), quit: make(chan struct{})}
	ru.watch(inst.Start())

	stopCtl, err := startControl(ru, resolved, cfg.UI)
	if err != nil {
		fmt.Fprintf(console, "crest: [warn] control channel unavailable (-s will not work): %v\n", err)
	} else {
		defer stopCtl()
	}

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-ru.errs:
		fmt.Fprintf(console, "crest: [emerg] %v\n", err)
		os.Exit(1)
	case sig := <-sigc:
		fmt.Fprintf(console, "crest: received %v, shutting down gracefully\n", sig)
	case <-ru.quit:
		fmt.Fprintln(console, "crest: stop requested via control channel, shutting down gracefully")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ru.shutdown(ctx); err != nil {
		fmt.Fprintf(console, "crest: shutdown: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "crest: [warn] cannot create log dir %s: %v\n", logDir, err)
		return server.Options{}, os.Stderr, func() {}
	}
	f, err := os.OpenFile(filepath.Join(logDir, "console.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "crest: [warn] cannot open console.log: %v\n", err)
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
	ui       *config.UIConfig // ui at startup; the control channel is fixed then
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
// reload returns a human-facing note (shown to the -s reload client) plus any
// fatal error. The note carries warnings the client should see, notably that
// ui-section changes need a full restart.
func (ru *runner) reload() (note string, err error) {
	cfg, err := loadConfig(ru.confPath)
	if err != nil {
		return "", err
	}
	for _, w := range cfg.Warnings {
		fmt.Fprintf(ru.console, "crest: [warn] %s\n", w)
	}
	// 控制通道（含 ui.allow / listen / token）在进程启动时固定，reload 不重建
	if !reflect.DeepEqual(ru.ui, cfg.UI) {
		note = "注意：ui 段已变更，但控制通道在进程启动时固定，需完全重启（-s stop 后重新启动）才生效"
		fmt.Fprintln(ru.console, "crest: [warn] "+note)
	}
	ni, err := server.NewWithOptions(cfg, ru.opts)
	if err != nil {
		return "", err
	}
	ru.mu.Lock()
	defer ru.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	ru.inst.Shutdown(ctx)
	cancel()
	ru.inst = ni
	ru.watch(ni.Start())
	fmt.Fprintln(ru.console, "crest: configuration reloaded")
	return note, nil
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
// port — inside the same crest process as the proxy itself. The address and
// an access token live in <conf>.ctl, which "crest -s <cmd> -c <conf>" reads.
// A "ui" config section pins the address/token and can auto-open the browser.
func startControl(ru *runner, confPath string, ui *config.UIConfig) (cleanup func(), err error) {
	listen := "127.0.0.1:0"
	var allow []netip.Prefix
	if ui != nil {
		var perr error
		if allow, perr = compileAllowList(ui.Allow); perr != nil {
			return nil, fmt.Errorf("ui.allow: %w", perr)
		}
	}
	if ui != nil && ui.Listen != "" {
		listen = ui.Listen
		if host, _, err := net.SplitHostPort(listen); err != nil || host == "" || host == "0.0.0.0" || host == "::" {
			if len(allow) == 0 {
				fmt.Fprintf(ru.console, "crest: [warn] ui.listen %q is not loopback-only and no ui.allow list is set; the console is reachable from the whole network (token is the only guard)\n", listen)
			} else {
				fmt.Fprintf(ru.console, "crest: [info] ui.listen %q restricted to allow list %v\n", listen, ui.Allow)
			}
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
	// 监听地址可能是通配（0.0.0.0 / ::），浏览器和 -s 命令都连不了，
	// 统一换成回环地址（通配监听必然包含 127.0.0.1）
	reachable := browsableAddr(ln.Addr().String())
	ctlPath := confPath + ".ctl"
	if err := os.WriteFile(ctlPath, []byte(reachable+" "+token), 0600); err != nil {
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
	mux := newControlMux(ru, token, auth)

	srv := &http.Server{Handler: withIPAllowlist(mux, allow)}
	go srv.Serve(ln)
	url := "http://" + reachable + "/ui?token=" + token
	fmt.Fprintf(ru.console, "crest: 管理页面 %s\n", url)
	if ui != nil && ui.Open {
		openBrowser(url)
	}
	return func() {
		srv.Close()
		os.Remove(ctlPath)
	}, nil
}

func newControlMux(ru *runner, token string, auth func(http.HandlerFunc) http.HandlerFunc) *http.ServeMux {
	mux := http.NewServeMux()
	registerUI(mux, ru, token, auth)
	mux.HandleFunc("/reload", auth(func(w http.ResponseWriter, r *http.Request) {
		note, err := ru.reload()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if note != "" {
			io.WriteString(w, "reloaded\n"+note+"\n")
		} else {
			io.WriteString(w, "reloaded\n")
		}
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
	mux.HandleFunc("/healthz", auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"status": "ok"})
	}))
	mux.HandleFunc("/readyz", auth(func(w http.ResponseWriter, r *http.Request) {
		ru.mu.Lock()
		ready := ru.inst.Ready()
		ru.mu.Unlock()
		if !ready {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]any{"status": "not_ready"})
			return
		}
		writeJSON(w, map[string]any{"status": "ready"})
	}))
	mux.HandleFunc("/status", auth(func(w http.ResponseWriter, r *http.Request) {
		st := controlStatus(ru)
		if strings.Contains(r.Header.Get("Accept"), "application/json") || r.URL.Query().Get("format") == "json" {
			writeJSON(w, st)
			return
		}
		fmt.Fprintf(w, "crest/%s pid=%d listening=%s\n", version, st["pid"], strings.Join(st["http_listeners"].([]string), ","))
	}))
	return mux
}

func controlStatus(ru *runner) map[string]any {
	ru.mu.Lock()
	inst, conf := ru.inst, ru.confPath
	ru.mu.Unlock()
	st := inst.Stats()
	addrs := make([]string, 0, len(inst.Addrs))
	by := make([]map[string]any, 0, len(inst.Addrs))
	for _, a := range inst.Addrs {
		addrs = append(addrs, a.Addr)
	}
	for _, a := range inst.StatsByAddr() {
		by = append(by, map[string]any{"addr": a.Addr, "total": a.Total, "c2xx": a.C2xx, "c3xx": a.C3xx, "c4xx": a.C4xx, "c5xx": a.C5xx})
	}
	return map[string]any{
		"version":          version,
		"pid":              os.Getpid(),
		"uptime":           int(time.Since(st.Started).Seconds()),
		"config_path":      conf,
		"http_listeners":   addrs,
		"stream_listeners": inst.StreamAddrs(),
		"ready":            inst.Ready(),
		"total":            st.Total,
		"c2xx":             st.C2xx,
		"c3xx":             st.C3xx,
		"c4xx":             st.C4xx,
		"c5xx":             st.C5xx,
		"by_addr":          by,
	}
}

// compileAllowList parses ui.allow entries (IP or CIDR) into prefixes.
func compileAllowList(entries []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, e := range entries {
		if strings.Contains(e, "/") {
			p, err := netip.ParsePrefix(e)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR %q: %w", e, err)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(e)
		if err != nil {
			return nil, fmt.Errorf("invalid address %q: %w", e, err)
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// withIPAllowlist gates the control channel by client IP. An empty list
// leaves access unrestricted (token remains the guard).
func withIPAllowlist(h http.Handler, allow []netip.Prefix) http.Handler {
	if len(allow) == 0 {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip, err := netip.ParseAddr(host)
		if err == nil {
			ip = ip.Unmap()
			for _, p := range allow {
				if p.Addr().Is4() == ip.Is4() && p.Contains(ip) {
					h.ServeHTTP(w, r)
					return
				}
			}
		}
		http.Error(w, "forbidden: your IP is not in the ui allow list", http.StatusForbidden)
	})
}

// browsableAddr turns a listen address into one a client can actually
// connect to: a wildcard host (0.0.0.0, ::, empty) becomes 127.0.0.1.
func browsableAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1:" + port
	}
	return net.JoinHostPort(host, port)
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
		return fmt.Errorf("cannot read control file %s.ctl (is crest running with -c %s?)", confPath, confPath)
	}
	parts := strings.Fields(string(data))
	if len(parts) != 2 {
		return errors.New("malformed control file")
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+parts[0]+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Crest-Token", parts[1])
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

// configNames 是省略 -c 时依次探测的文件名，让双击启动的 exe 无需任何参数
// 就能找到自己的配置。YAML 在前：它是推荐格式，也是双击场景下的常见形态。
var configNames = []string{"crest.yml", "crest.yaml", "crest.conf"}

// resolveConfigPath 返回实际使用的配置路径。显式 -c 原样返回；省略时在工作
// 目录、再在可执行文件所在目录里按 configNames 顺序查找 —— 双击启动时这两
// 处是配置可能存放的位置（从资源管理器双击通常是前者，从快捷方式启动则可能
// 只有后者成立）。autoFound 标识是否来自自动探测，供启动日志说明来源。
func resolveConfigPath(explicit string) (path string, autoFound bool, err error) {
	if explicit != "" {
		return explicit, false, nil
	}
	p, err := findConfig(configSearchDirs())
	return p, true, err
}

// configSearchDirs 依次是工作目录与可执行文件所在目录，去重后返回。
func configSearchDirs() []string {
	var dirs []string
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		if len(dirs) == 0 || d != dirs[0] {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// findConfig 在给定目录里按 configNames 顺序找第一个存在的配置文件；都没有
// 时，错误信息列出全部找过的路径，免得使用者只看到一句"找不到配置"。
func findConfig(dirs []string) (string, error) {
	var tried []string
	for _, dir := range dirs {
		for _, name := range configNames {
			p := filepath.Join(dir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, nil
			}
			tried = append(tried, p)
		}
	}
	if len(tried) == 0 {
		return "", errors.New("no configuration file given and none could be looked for; use -c")
	}
	return "", fmt.Errorf("no configuration file found. Looked for:\n  %s\nPass one with -c, or put a %s next to the executable",
		strings.Join(tried, "\n  "), configNames[0])
}

func loadConfig(path string) (*config.Config, error) {
	return config.LoadFile(path)
}
