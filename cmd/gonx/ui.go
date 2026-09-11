package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gonx/internal/config"
)

// registerUI mounts the browser console and its JSON API on the control mux.
// auth wraps handlers with token verification (header or cookie).
func registerUI(mux *http.ServeMux, ru *runner, token string, auth func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("/ui", func(w http.ResponseWriter, r *http.Request) {
		// first visit carries ?token= from "gonx -s ui"; move it into a cookie
		if q := r.URL.Query().Get("token"); q != "" {
			if q != token {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "gonx_token", Value: q, Path: "/", HttpOnly: true})
			http.Redirect(w, r, "/ui", http.StatusFound)
			return
		}
		if !tokenOK(r, token) {
			http.Error(w, "forbidden: open the console with \"gonx -s ui\"", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, uiHTML)
	})

	mux.HandleFunc("/api/status", auth(func(w http.ResponseWriter, r *http.Request) {
		ru.mu.Lock()
		inst := ru.inst
		ru.mu.Unlock()
		st := inst.Stats()
		addrs := make([]string, 0, len(inst.Addrs))
		for _, a := range inst.Addrs {
			addrs = append(addrs, a.Addr)
		}
		byAddr := make([]map[string]any, 0, len(addrs))
		for _, as := range inst.StatsByAddr() {
			byAddr = append(byAddr, map[string]any{
				"addr": as.Addr, "total": as.Total,
				"c2xx": as.C2xx, "c3xx": as.C3xx, "c4xx": as.C4xx, "c5xx": as.C5xx,
			})
		}
		writeJSON(w, map[string]any{
			"version": version,
			"pid":     os.Getpid(),
			"uptime":  int(time.Since(st.Started).Seconds()),
			"conf":    ru.confPath,
			"addrs":   addrs,
			"streams": inst.StreamAddrs(),
			"total":   st.Total,
			"c2xx":    st.C2xx,
			"c3xx":    st.C3xx,
			"c4xx":    st.C4xx,
			"c5xx":    st.C5xx,
			"byAddr":  byAddr,
		})
	}))

	mux.HandleFunc("/api/logs", auth(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		switch name {
		case "success", "error", "console":
		default:
			http.Error(w, "unknown log", http.StatusBadRequest)
			return
		}
		if ru.opts.LogDir == "" {
			http.Error(w, "log directory disabled", http.StatusNotFound)
			return
		}
		text, err := tailFile(filepath.Join(ru.opts.LogDir, name+".log"), 200)
		if err != nil {
			text = "(暂无日志)"
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, text)
	}))

	mux.HandleFunc("/api/logs/clear", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := r.URL.Query().Get("name")
		switch name {
		case "success", "error", "console":
		default:
			http.Error(w, "unknown log", http.StatusBadRequest)
			return
		}
		if ru.opts.LogDir == "" {
			http.Error(w, "log directory disabled", http.StatusNotFound)
			return
		}
		// 写入方一直以 O_APPEND 持有该文件，原地截断即可重置：
		// 下一次写入自然落到偏移 0，无需关闭/重开，也不会留下空洞。
		if err := os.Truncate(filepath.Join(ru.opts.LogDir, name+".log"), 0); err != nil && !os.IsNotExist(err) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		io.WriteString(w, "已清空 "+name+".log")
	}))

	mux.HandleFunc("/api/config", auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			data, err := os.ReadFile(ru.confPath)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"path": ru.confPath, "content": string(data)})
		case http.MethodPost:
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			// validate through the real loader before touching the live file
			tmp, err := os.CreateTemp("", "gonx-*"+filepath.Ext(ru.confPath))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			tmpName := tmp.Name()
			tmp.Write(body)
			tmp.Close()
			_, verr := config.LoadFile(tmpName)
			os.Remove(tmpName)
			if verr != nil {
				http.Error(w, "配置校验失败: "+verr.Error(), http.StatusBadRequest)
				return
			}
			if old, err := os.ReadFile(ru.confPath); err == nil {
				// The backup is a full copy of the config, which typically
				// carries ui.token — mirror the original's mode rather than
				// creating a world-readable 0644 copy beside a 0600 original.
				mode := os.FileMode(0600)
				if fi, serr := os.Stat(ru.confPath); serr == nil {
					mode = fi.Mode().Perm()
				}
				os.WriteFile(ru.confPath+".bak", old, mode)
			}
			if err := os.WriteFile(ru.confPath, body, 0644); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			io.WriteString(w, "已保存（原文件备份为 .bak）")
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
}

func tokenOK(r *http.Request, token string) bool {
	if r.Header.Get("X-Gonx-Token") == token {
		return true
	}
	if c, err := r.Cookie("gonx_token"); err == nil && c.Value == token {
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

// tailFile returns the last n lines, reading at most the final 256KB.
func tailFile(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	const window = 256 << 10
	if fi.Size() > window {
		f.Seek(fi.Size()-window, io.SeekStart)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), nil
}

func openBrowser(url string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		exec.Command("open", url).Start()
	default:
		exec.Command("xdg-open", url).Start()
	}
}

// openUI reads the control file and launches the browser console.
func openUI(confPath string) error {
	data, err := os.ReadFile(confPath + ".ctl")
	if err != nil {
		return fmt.Errorf("cannot read control file %s.ctl (is gonx running with -c %s?)", confPath, confPath)
	}
	parts := strings.Fields(string(data))
	if len(parts) != 2 {
		return fmt.Errorf("malformed control file")
	}
	url := "http://" + parts[0] + "/ui?token=" + parts[1]
	openBrowser(url)
	fmt.Println("管理页面: " + url)
	return nil
}

const uiHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>gonx 控制台</title>
<link rel="icon" type="image/png" href="data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAACAAAAAgCAYAAABzenr0AAACsUlEQVR4nMSXTWgTURDH/83GJpaGRqqQthZXRBR60JQWFaSNXgwWJEcPfgRz9JLeAiJG6sHjFkS8ROPRi6QHxV7StaVSscQPECo9dIvatNTWxkgaa6syDxfi5u1HGrP53fLeZOf/dmbe7DihslNowem2KI7vCaPVLaIWrBQVTC0nkc5KWN/K0VID29jbdATRLhlNTm9NHGspbK5Beh/Ap8LbBnbyoW7FNuelIq5nRAFnOmLo2hW01Tmxw+HG5q+igEsHkrafXqXVJTorTbh9Hh9CYh+O7j4I0dPG1pR8Fm++zCKljGM+v1iBALfYgLsnflux7W/3I94TQaCj29BO/pxBfDqB5wuvLWmwJOBGTwTx3oilB6rEXyVwczphaucwM3hw6lrFzpmA3gj7b1UC6OThwwMVO1fxujxoaWw2tHHqbbCY65w8OfMEqblxyH/jHGj3I7S/7x+x0rtHGJwcNhWpmwNj5+6UJdzajzxCz2K6CUaiU8HbiE4O4+GHp6bOdQVQqSkXHpcZB0auWs5uq3BDQHWuhV47zznliRXkhQz3/1wBdMlooZjzsFohyRkfVwC3CtQbrhS5ylfPe6auADvhCqC7XQuVWjXwngm9HKDGooXqfESZKFunK9cKlIQ86l6G3BBQS6WupoUumX6DUNDe1yujuHzorGUBAgY647wNipm2D7idLrYmenzs92JhFW6hEcHOY4j5L0I6GWU2FC6vqxmjH1+aCjBsx9tpw6XQ3RFO30Ju47uujWEZUj+nG3C7UO8wcg6jEKiomW/2JaSFqmPwRRXdUEtdP8lK+a8fpUzAkH+uZqOYGStFxcFmtXoxtZx0sEGRxiS7IZ/prORgUyoNinaKUIfT9a2cwBa+/VzCxNI9mtVoXKrZqEbj+VhWwv3Z81jdmKelPwEAAP//9W0em/Nyz1MAAAAASUVORK5CYII=">
<style>
:root {
  --bg: #f6f7f9; --surface: #ffffff; --border: #e3e6ea;
  --ink: #1c2733; --ink-2: #5b6b7b; --ink-3: #8a97a5;
  --accent: #009639; --accent-ink: #ffffff;
  --danger: #c62828; --warn: #b26a00;
  --mono: ui-monospace, Consolas, "Courier New", monospace;
  /* VS Code Light+ 配色 */
  --tk-key: #0451a5; --tk-str: #a31515; --tk-com: #008000;
  --tk-num: #098658; --tk-bool: #0000ff; --tk-var: #af00db;
  --tk-pun: #0431fa;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #14181d; --surface: #1d232b; --border: #2c353f;
    --ink: #e4e9ee; --ink-2: #9aa8b6; --ink-3: #6b7885;
    --accent: #23b35c; --danger: #ef5350; --warn: #ffb74d;
    /* VS Code Dark+ 配色 */
    --tk-key: #9cdcfe; --tk-str: #ce9178; --tk-com: #6a9955;
    --tk-num: #b5cea8; --tk-bool: #569cd6; --tk-var: #c586c0;
    --tk-pun: #ffd700;
  }
}
* { box-sizing: border-box; margin: 0; }
body { background: var(--bg); color: var(--ink); font: 14px/1.6 "Segoe UI", system-ui, sans-serif; padding: 24px; }
.wrap { max-width: 1160px; margin: 0 auto; display: grid; gap: 16px; }
.wrap > * { min-width: 0; } /* 防止长行内容撑宽 grid 导致页面偏移 */
header { display: flex; align-items: baseline; gap: 12px; flex-wrap: wrap; }
header h1 { font-size: 20px; }
header .meta { color: var(--ink-2); font-size: 13px; }
.dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; background: var(--accent); margin-right: 6px; }
.dot.stopped { background: var(--danger); }
.card { background: var(--surface); border: 1px solid var(--border); border-radius: 10px; padding: 16px 18px; }
.card h2 { font-size: 14px; color: var(--ink-2); font-weight: 600; margin-bottom: 12px; }
.tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(120px, 1fr)); gap: 10px; }
.tile { border: 1px solid var(--border); border-radius: 8px; padding: 10px 14px; }
.tile .num { font-size: 24px; font-weight: 650; font-variant-numeric: tabular-nums; }
.tile .lbl { font-size: 12px; color: var(--ink-3); }
.tile.err .lbl::before { content: "●"; color: var(--danger); margin-right: 4px; font-size: 9px; vertical-align: 1px; }
.tile.warn .lbl::before { content: "●"; color: var(--warn); margin-right: 4px; font-size: 9px; vertical-align: 1px; }
.btns { display: flex; gap: 10px; flex-wrap: wrap; }
button { border: 1px solid var(--border); background: var(--surface); color: var(--ink);
  border-radius: 8px; padding: 8px 16px; font-size: 14px; cursor: pointer; }
button:hover { border-color: var(--accent); }
button.primary { background: var(--accent); border-color: var(--accent); color: var(--accent-ink); }
button.danger { color: var(--danger); }
button.danger:hover { border-color: var(--danger); }
.tabs { display: flex; gap: 4px; margin-bottom: 10px; }
.tabs button { padding: 5px 14px; border-radius: 6px; font-size: 13px; }
.tabs button.on { background: var(--accent); border-color: var(--accent); color: var(--accent-ink); }
.dropdown { float: right; position: relative; font-weight: 400; }
.dd-btn { display: inline-flex; align-items: center; gap: 8px; font-size: 12px;
  font-variant-numeric: tabular-nums; padding: 5px 10px 5px 12px; min-width: 96px;
  border: 1px solid var(--border); border-radius: 8px; background: var(--surface); color: var(--ink);
  cursor: pointer; transition: border-color .12s, box-shadow .12s; }
.dd-btn:hover { border-color: var(--ink-3); }
.dropdown.open .dd-btn { border-color: var(--accent); box-shadow: 0 0 0 2px rgba(0,150,57,.18); }
.dd-btn .cur { flex: 1; text-align: left; }
.dd-btn .arw { width: 12px; height: 12px; flex: none; color: var(--ink-3); transition: transform .15s; }
.dropdown.open .dd-btn .arw { transform: rotate(180deg); }
.dd-menu { position: absolute; right: 0; top: calc(100% + 5px); z-index: 60; min-width: 100%;
  max-height: 300px; overflow: auto; padding: 4px; background: var(--surface);
  border: 1px solid var(--border); border-radius: 8px; box-shadow: 0 8px 28px rgba(0,0,0,.18); }
.dd-menu button { display: flex; align-items: center; gap: 6px; width: 100%; text-align: left;
  white-space: nowrap; padding: 6px 10px; border: none; border-radius: 6px; background: transparent;
  color: var(--ink); font-size: 12px; font-variant-numeric: tabular-nums; cursor: pointer; }
.dd-menu button:hover { background: var(--bg); }
.dd-menu button.on { color: var(--accent); font-weight: 600; }
.dd-menu button .ck { width: 12px; flex: none; opacity: 0; }
.dd-menu button.on .ck { opacity: 1; }
pre { background: var(--bg); border: 1px solid var(--border); border-radius: 8px; padding: 12px;
  font: 12px/1.5 var(--mono); overflow: auto; max-height: 320px; white-space: pre-wrap; word-break: break-all; }
.logbox { display: flex; border: 1px solid var(--border); border-radius: 8px; overflow: hidden;
  height: 320px; background: var(--bg); }
.logbox .gutter { padding: 12px 0; }
#loglnums { font: 12px/1.5 var(--mono); color: var(--ink-3); text-align: right;
  padding: 0 8px 0 12px; min-width: 2.6em; white-space: pre; user-select: none; }
#log { flex: 1; min-width: 0; margin: 0; border: none; border-radius: 0; max-height: none;
  white-space: pre; word-break: normal; background: transparent; }
.editor { display: flex; height: 62vh; min-height: 420px; border: 1px solid var(--border);
  border-radius: 8px; overflow: hidden; background: var(--bg); }
.gutter { flex: none; padding: 14px 0; border-right: 1px solid var(--border); overflow: hidden;
  background: var(--surface); }
#lnums { font: 13px/1.6 var(--mono); color: var(--ink-3); text-align: right;
  padding: 0 10px 0 14px; min-width: 3.2em; white-space: pre; user-select: none; }
.edwrap { position: relative; flex: 1; overflow: hidden; min-width: 0; }
#hl, #conf { position: absolute; inset: 0; margin: 0; padding: 14px; font: 13px/1.6 var(--mono);
  white-space: pre; overflow: auto; tab-size: 2; border: none; border-radius: 0;
  background: transparent; max-height: none; }
#hl { pointer-events: none; color: var(--ink); }
#hl .tk { color: var(--tk-key); } #hl .ts { color: var(--tk-str); }
#hl .tc { color: var(--tk-com); font-style: italic; } #hl .tn { color: var(--tk-num); }
#hl .tb { color: var(--tk-bool); } #hl .tv { color: var(--tk-var); }
#hl .tp { color: var(--tk-pun); }
#conf { color: transparent; caret-color: var(--ink); resize: none; outline: none; width: 100%; height: 100%; }
#conf::selection { background: rgba(0, 150, 57, .28); color: transparent; }
.help-btn { float: right; font-size: 12px; padding: 3px 10px; }
.help { position: fixed; top: 0; right: 0; bottom: 0; width: min(640px, 94vw); z-index: 50;
  background: var(--surface); border-left: 1px solid var(--border);
  box-shadow: -12px 0 32px rgba(0,0,0,.18); padding: 16px; overflow: auto; }
.help-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 10px; }
.help-head b { font-size: 15px; }
.help-head button { padding: 2px 10px; font-size: 14px; line-height: 1.4; }
.help input { width: 100%; padding: 6px 10px; margin-bottom: 8px; border: 1px solid var(--border);
  border-radius: 6px; background: var(--surface); color: var(--ink); font-size: 13px; }
.help details { margin-bottom: 4px; }
.help summary { cursor: pointer; font-weight: 600; font-size: 13px; padding: 4px 0; }
.help table { width: 100%; border-collapse: collapse; font-size: 12px; margin: 6px 0 10px;
  table-layout: fixed; }
.help th, .help td { text-align: left; padding: 4px 8px; border-bottom: 1px solid var(--border);
  vertical-align: top; word-break: break-word; overflow-wrap: anywhere; }
.help th { color: var(--ink-3); font-weight: 600; }
.help th:nth-child(1), .help td:nth-child(1) { width: 29%; }
.help th:nth-child(2), .help td:nth-child(2) { width: 42%; }
.help td:first-child { font-family: var(--mono); }
.help td:last-child { font-family: var(--mono); color: var(--ink-2); }
.help .note { font-size: 12px; color: var(--ink-2); margin: 4px 0 8px; }
.msg { font-size: 13px; margin-left: 8px; }
.msg.ok { color: var(--accent); } .msg.err { color: var(--danger); white-space: pre-wrap; }
.msg.warn { color: var(--warn); }
label.auto { font-size: 13px; color: var(--ink-2); margin-left: auto; user-select: none; }
.tabs .clear-log { margin-left: 10px; }
.row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
code { font-family: var(--mono); font-size: 12px; color: var(--ink-2); }
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1><span class="dot" id="dot"></span>gonx 控制台</h1>
    <span class="meta" id="meta">连接中…</span>
  </header>

  <div class="card">
    <h2>请求统计（自本次启动）
      <span class="dropdown" id="portdd">
        <button type="button" class="dd-btn" onclick="toggleDD(event)">
          <span class="cur" id="dd-label">全部端口</span>
          <svg class="arw" viewBox="0 0 12 12"><path fill="none" stroke="currentColor" stroke-width="1.4"
            stroke-linecap="round" stroke-linejoin="round" d="M2.5 4.5 6 8l3.5-3.5"/></svg>
        </button>
        <div class="dd-menu" id="dd-menu" hidden></div>
      </span></h2>
    <div class="tiles">
      <div class="tile"><div class="num" id="t-total">–</div><div class="lbl">总请求</div></div>
      <div class="tile"><div class="num" id="t-2xx">–</div><div class="lbl">2xx 成功</div></div>
      <div class="tile"><div class="num" id="t-3xx">–</div><div class="lbl">3xx 跳转</div></div>
      <div class="tile warn"><div class="num" id="t-4xx">–</div><div class="lbl">4xx 客户端错误</div></div>
      <div class="tile err"><div class="num" id="t-5xx">–</div><div class="lbl">5xx 服务端错误</div></div>
    </div>
  </div>

  <div class="card">
    <h2>操作</h2>
    <div class="row btns">
      <button class="primary" onclick="act('/reload', this)">重载配置</button>
      <button onclick="act('/reopen', this)">重开日志文件</button>
      <button class="danger" onclick="stopServer(this)">停止服务</button>
      <span class="msg" id="act-msg"></span>
    </div>
  </div>

  <div class="card">
    <h2>日志</h2>
    <div class="row tabs">
      <button class="on" data-log="success" onclick="pickLog(this)">success.log</button>
      <button data-log="error" onclick="pickLog(this)">error.log</button>
      <button data-log="console" onclick="pickLog(this)">console.log</button>
      <span class="msg" id="log-msg"></span>
      <label class="auto"><input type="checkbox" id="auto" checked> 自动刷新</label>
      <button class="danger clear-log" onclick="clearLog(this)">清空</button>
    </div>
    <div class="logbox">
      <div class="gutter"><div id="loglnums">1</div></div>
      <pre id="log" onscroll="logSync()">载入中…</pre>
    </div>
  </div>

  <div class="card">
    <h2>配置文件 <code id="conf-path"></code>
      <button class="help-btn" onclick="toggleHelp()">配置帮助</button></h2>
    <div id="help" class="help" hidden>
      <div class="help-head"><b>配置帮助</b><button onclick="toggleHelp()" title="关闭">✕</button></div>
      <input id="help-q" placeholder="搜索配置项，如 proxy_pass、limit、ssl…" oninput="filterHelp()">
      <div class="note">单位：大小 <b>1k / 200m / 1g</b>；时间 <b>30s / 10m / 2h / 1d</b>；速率 <b>5r/s / 60r/m</b>。未知字段会在保存校验时直接报错。</div>

      <details open><summary>完整配置示例（含全部配置项，带注释）</summary>
<pre># ===== 管理页面（可选）=====
ui:
  listen: 127.0.0.1:8899        # 管理页面地址；回环或具体本机 IP 已限制访问面
  token: change-me-please       # 访问令牌（不写则每次启动随机生成）
  open: true                    # 启动时自动打开浏览器（无桌面环境的服务器会静默跳过）
  allow: [127.0.0.1, 192.168.0.0/16]   # IP 白名单（仅绑 0.0.0.0 时才需要）

# ===== 进程错误日志 =====
error_log:
  path: logs/error.log          # 路径；也可写 stderr / stdout
  level: info                   # debug / info / warn / error

# ===== HTTP 服务 =====
http:
  access_log: {path: logs/access.log, format: main}   # 不写则自动分流到 log/success.log 与 log/error.log
  log_formats:
    main: '$remote_addr - [$time_local] "$request" $status $body_bytes_sent $request_time $upstream_addr'

  gzip:                         # 动态压缩
    enabled: true
    min_length: 1k
    level: 6
    types: [text/css, application/javascript, application/json]
  gzip_static: true             # 存在 &lt;文件&gt;.gz 时直接发预压缩文件

  limit_req_zones:              # 限速区（漏桶）
    perip: {key: $binary_remote_addr, rate: 10r/s}
  limit_conn_zones:             # 并发连接数区
    addr: $binary_remote_addr
  cache_paths:                  # 代理缓存区
    apicache: cache/api

  maps:                         # 变量映射
    - source: $http_upgrade
      target: $conn_upgrade
      cases: {default: "", websocket: upgrade}

  # 以下为全局默认，server / location 就近覆盖
  client_max_body_size: 50m
  proxy_connect_timeout: 60s    # 连接后端超时
  proxy_read_timeout: 300s      # 读后端响应超时
  proxy_send_timeout: 60s       # 写后端请求超时
  proxy_buffering: true         # 缓冲后端响应（默认开）；关掉则直通不缓冲
  proxy_buffers: {count: 8, size: 32k}   # 每个响应的内存预算，超出转存临时文件
  proxy_max_temp_file_size: 1g  # 临时文件上限；0 表示只用内存、不落盘
  limit_rate: 0                 # 响应带宽限速（0=不限）
  autoindex: false
  server_tokens: false          # Server 头不带版本号
  ssl_protocols: [TLSv1.2, TLSv1.3]

  upstreams:                    # 负载均衡后端组
    backend:
      balancer: least_conn      # round_robin(默认) / least_conn / ip_hash
      keepalive: 32             # 到后端的空闲长连接池
      servers:
        - {address: 127.0.0.1:8001, weight: 3, max_fails: 2, fail_timeout: 30s}
        - {address: 127.0.0.1:8002}
        - {address: 127.0.0.1:8003, backup: true}
        - {address: 127.0.0.1:8004, down: true}

  servers:
    # ---- 站点 1：静态站 + SPA + 反代 + HTTPS ----
    - listen: [80, {port: 443, ssl: true, default: true}]
      server_name: [example.com, "*.example.com"]
      root: /var/www/dist
      index: [index.html, index.htm]
      ssl_certificate: certs/example.crt
      ssl_certificate_key: certs/example.key
      error_pages:
        - {codes: [500, 502, 503, 504], uri: /50x.html}
      add_headers:
        - {name: X-Frame-Options, value: SAMEORIGIN, always: true}
      access:
        - {allow: 192.168.0.0/16}
        - {deny: all}
      rewrites:
        - {pattern: "^/old/(.*)$", replacement: /new/$1, flag: permanent}

      locations:
        - path: /
          try_files: [$uri, $uri/, /index.html]

        - path: /api/
          proxy_pass: http://backend
          proxy_set_header:
            Host: $host
            X-Real-IP: $remote_addr
            X-Forwarded-For: $proxy_add_x_forwarded_for
            X-Forwarded-Proto: $scheme
          proxy_cookie_path:
            - {from: /api/, to: /}
          proxy_redirect:
            - {from: 'http://backend/', to: /api/}
          proxy_hide_header: [X-Powered-By]
          proxy_pass_header: [Server]
          proxy_cache: apicache
          proxy_cache_valid:
            - {codes: [200, 301], time: 10m}
            - {any: true, time: 1m}
          proxy_connect_timeout: 5s
          proxy_read_timeout: 120s
          proxy_send_timeout: 30s
          limit_req: {zone: perip, burst: 20, nodelay: true}
          limit_conn: {zone: addr, max: 10}
          client_max_body_size: 200m

        - path: /download/
          alias: /var/www/files/
          autoindex: true
          limit_rate: 500k              # 下载限速 500KB/s
          limit_rate_after: 1m          # 前 1MB 全速
          gzip: {enabled: false}
          expires: 7d

        - path: /img/
          root: /var/www
          valid_referers: [none, server_names, "*.trusted.com"]   # 防盗链
          expires: max

        - path: /admin/
          auth_basic: {realm: "Admin Area", user_file: certs/htpasswd}

        - path: /ping
          match: exact
          return: {code: 200, body: "pong $scheme"}

        - path: "@fallback"                # 命名 location
          proxy_pass: http://backend

    # ---- 站点 2：正则匹配 ----
    - listen: [8080]
      server_name: ['~^api\d+\.example\.com$']
      locations:
        - path: '\.(jpg|png|gif)$'
          match: regex_i
          root: /var/www/images
          expires: 30d

# ===== L4 代理（TCP / UDP）=====
stream:
  upstreams:
    tcp_pool:
      servers:
        - {address: 127.0.0.1:5432}
  servers:
    - {listen: 15432, proxy_pass: tcp_pool, proxy_timeout: 1h}
    - {listen: 19100, udp: true, proxy_pass: 192.168.1.100:19100, proxy_responses: 1, proxy_timeout: 10s}</pre>
      <div class="note">这是覆盖全部指令的参考配置，直接复制后按实际情况修改路径/域名/后端地址即可；不需要的段落删掉。保存为 gonx.yml 后运行 gonx -c gonx.yml；先用 gonx -t -c gonx.yml 校验（占位路径会提示不存在，属正常）。</div>
      </details>

      <details><summary>顶层结构</summary>
      <table><tr><th>字段</th><th>说明</th><th>示例</th></tr>
      <tr><td>ui</td><td>管理页面：listen 地址、token 令牌、open 自动开浏览器、allow IP 白名单（仅绑 0.0.0.0 时才需要；回环或具体 IP 已限制访问面）。改后需重启</td><td>listen: 127.0.0.1:8899</td></tr>
      <tr><td>error_log</td><td>进程错误日志路径与级别（debug/info/warn/error）</td><td>{path: stderr, level: info}</td></tr>
      <tr><td>http</td><td>HTTP 服务：servers/upstreams/全局指令</td><td></td></tr>
      <tr><td>stream</td><td>L4 代理（TCP/UDP）</td><td></td></tr></table></details>

      <details><summary>http 全局</summary>
      <table><tr><th>字段</th><th>说明</th><th>示例</th></tr>
      <tr><td>access_log</td><td>访问日志；不写则自动分流到 log/success.log 与 log/error.log</td><td>{path: a.log, format: main} 或 {off: true}</td></tr>
      <tr><td>log_formats</td><td>自定义日志格式（内置 combined）</td><td>main: '$remote_addr $status'</td></tr>
      <tr><td>gzip</td><td>压缩开关/类型/阈值/级别</td><td>{enabled: true, min_length: 1k}</td></tr>
      <tr><td>limit_req_zones</td><td>限速区：key + 速率</td><td>perip: {key: $binary_remote_addr, rate: 5r/s}</td></tr>
      <tr><td>limit_conn_zones</td><td>并发区：名称 → key 变量</td><td>addr: $binary_remote_addr</td></tr>
      <tr><td>cache_paths</td><td>代理缓存区：名称 → 磁盘目录</td><td>zone1: cache_dir</td></tr>
      <tr><td>maps</td><td>变量映射（支持 ~正则 / ~*忽略大小写 / default）</td><td>{source: $host, target: $pool, cases: {...}}</td></tr>
      <tr><td>root / index / autoindex …</td><td>可在 http 层给出默认值，server/location 就近覆盖</td><td></td></tr></table></details>

      <details><summary>servers（虚拟主机列表）</summary>
      <table><tr><th>字段</th><th>说明</th><th>示例</th></tr>
      <tr><td>listen</td><td>监听端口列表；每项可为端口、"端口 ssl default" 简写，或 {port, ssl, default} 映射。ssl 是每项独立属性，同一端口不能既明文又 ssl</td><td>[80, {port: 443, ssl: true}]</td></tr>
      <tr><td>server_name</td><td>按 Host 匹配：精确 / *.foo / foo.* / ~正则</td><td>[example.com, "*.example.org"]</td></tr>
      <tr><td>root / index</td><td>静态根目录与默认页</td><td>root: dist</td></tr>
      <tr><td>ssl_certificate(_key)</td><td>PEM 证书 + 私钥；多 server 自动按 SNI 选择；TLS 下自动启用 HTTP/2</td><td>ssl_certificate: cert.pem</td></tr>
      <tr><td>ssl_protocols</td><td>允许的 TLS 版本（缺省最低 TLSv1.2）</td><td>[TLSv1.2, TLSv1.3]</td></tr>
      <tr><td>server_tokens</td><td>Server 响应头：true 带版本 / false 仅 gonx</td><td>server_tokens: false</td></tr>
      <tr><td>error_pages</td><td>自定义错误页（仅拦截 gonx 自身错误，上游错误透传）</td><td>[{codes: [502,503], uri: /50x.html}]</td></tr>
      <tr><td>rewrites</td><td>server 级重写，location 匹配前执行</td><td>见 location</td></tr>
      <tr><td>locations</td><td>路由规则列表</td><td></td></tr></table></details>

      <details><summary>locations（路由列表）</summary>
      <table><tr><th>字段</th><th>说明</th><th>示例</th></tr>
      <tr><td>path + match</td><td>匹配方式：prefix(默认) / exact / regex / regex_i / prefix_priority（对应 nginx 空 / = / ~ / ~* / ^~）</td><td>{path: /ping, match: exact}</td></tr>
      <tr><td>path: "@名字"</td><td>命名 location，仅由 try_files 跳转进入</td><td>try_files: [$uri, "@backend"]</td></tr>
      <tr><td>proxy_pass</td><td>反向代理；host 为 upstream 名或地址；带尾部 / 时替换匹配前缀；WebSocket 自动升级</td><td>http://pool/</td></tr>
      <tr><td>proxy_set_header</td><td>传给后端的请求头（支持变量）</td><td>{Host: $host, X-Real-IP: $remote_addr}</td></tr>
      <tr><td>proxy_cookie_path</td><td>改写后端 Set-Cookie 的 Path 前缀</td><td>[{from: /api/, to: /}]</td></tr>
      <tr><td>proxy_redirect</td><td>改写后端 3xx 的 Location 头前缀；off 关闭，缺省按 proxy_pass→location 自动改</td><td>[{from: 'http://backend/', to: /api/}]</td></tr>
      <tr><td>proxy_hide_header / proxy_pass_header</td><td>隐藏 / 放行后端响应头（默认已隐藏 Date/Server/X-Accel-*）</td><td>proxy_hide_header: [X-Secret]</td></tr>
      <tr><td>expires</td><td>加缓存头：时长(30d) / off / epoch / max / 负值(no-cache)</td><td>expires: 7d</td></tr>
      <tr><td>limit_rate / limit_rate_after</td><td>响应带宽限速；前 N 字节不限速</td><td>limit_rate: 200k</td></tr>
      <tr><td>gzip_static</td><td>存在 &lt;文件&gt;.gz 且客户端支持时直接发预压缩文件</td><td>gzip_static: true</td></tr>
      <tr><td>valid_referers</td><td>防盗链：非法 Referer 返回 403；规则 none/blocked/server_names/域名/~正则</td><td>[none, server_names, "*.trusted.com"]</td></tr>
      <tr><td>return</td><td>直接响应；30x + body 为跳转</td><td>{code: 200, body: "ok $scheme"}</td></tr>
      <tr><td>rewrites</td><td>正则重写；flag: last / break / redirect / permanent；$1-$9 捕获组</td><td>{pattern: "^/old/(.*)$", replacement: /new/$1, flag: permanent}</td></tr>
      <tr><td>try_files</td><td>依次探测文件，末项为兜底（URI / =404 / @named）；SPA 常用</td><td>[$uri, $uri/, /index.html]</td></tr>
      <tr><td>alias</td><td>替换匹配前缀映射目录（区别于 root 拼接全路径）</td><td>alias: /var/assets/</td></tr>
      <tr><td>proxy_cache(_valid)</td><td>启用缓存区 + TTL；响应带 X-Cache-Status</td><td>proxy_cache_valid: [{codes: [200], time: 10m}]</td></tr>
      <tr><td>limit_req / limit_conn</td><td>限速（burst/nodelay）/ 并发上限</td><td>{zone: perip, burst: 10, nodelay: true}</td></tr></table></details>

      <details><summary>通用指令（http / server / location 就近覆盖）</summary>
      <table><tr><th>字段</th><th>说明</th><th>示例</th></tr>
      <tr><td>add_headers</td><td>加响应头；always 时错误响应也加</td><td>[{name: X-Foo, value: bar, always: true}]</td></tr>
      <tr><td>access</td><td>IP 访问控制，按顺序首个命中生效</td><td>[{allow: 10.0.0.0/8}, {deny: all}]</td></tr>
      <tr><td>auth_basic</td><td>基础认证；htpasswd 支持 bcrypt/apr1/SHA1/明文</td><td>{realm: Admin, user_file: htpasswd}</td></tr>
      <tr><td>autoindex</td><td>目录列表开关</td><td>autoindex: true</td></tr>
      <tr><td>client_max_body_size</td><td>请求体上限，超出 413</td><td>200m</td></tr>
      <tr><td>proxy_connect_timeout / proxy_read_timeout / proxy_send_timeout</td><td>连接/读/写后端超时</td><td>proxy_read_timeout: 300s</td></tr>
      <tr><td>proxy_buffering</td><td>缓冲后端响应，慢客户端不再占住后端连接（默认开，同 nginx）。SSE/流式响应不受影响：有数据即转发，不等缓冲区填满</td><td>proxy_buffering: false</td></tr>
      <tr><td>proxy_buffers</td><td>单个响应的内存预算 = count × size，超出部分异步转存临时文件</td><td>{count: 4, size: 256k}</td></tr>
      <tr><td>proxy_max_temp_file_size</td><td>临时文件上限；0 = 不落盘（超出内存即退化为直通背压）</td><td>1g</td></tr>
      <tr><td>gzip</td><td>同 http 层，可局部覆盖</td><td>{enabled: false}</td></tr></table></details>

      <details><summary>upstreams（负载均衡）</summary>
      <table><tr><th>字段</th><th>说明</th><th>示例</th></tr>
      <tr><td>balancer</td><td>round_robin(默认加权轮询) / least_conn / ip_hash</td><td>balancer: least_conn</td></tr>
      <tr><td>keepalive</td><td>到后端的空闲长连接数（连接复用，降低握手开销）</td><td>keepalive: 32</td></tr>
      <tr><td>servers[].address</td><td>后端地址（无端口默认 :80）</td><td>127.0.0.1:9001</td></tr>
      <tr><td>weight</td><td>权重，默认 1</td><td>weight: 3</td></tr>
      <tr><td>max_fails / fail_timeout</td><td>窗口内失败 N 次摘除 fail_timeout（默认 1 次 / 10s）</td><td>max_fails: 2, fail_timeout: 30s</td></tr>
      <tr><td>backup / down</td><td>备用节点（主全挂才启用）/ 手动摘除</td><td>backup: true</td></tr></table></details>

      <details><summary>stream（L4 代理）</summary>
      <table><tr><th>字段</th><th>说明</th><th>示例</th></tr>
      <tr><td>servers[].listen + udp</td><td>监听端口；udp: true 走 UDP</td><td>{listen: 19100, udp: true}</td></tr>
      <tr><td>proxy_pass</td><td>upstream 名或 host:port</td><td>192.168.3.100:19100</td></tr>
      <tr><td>proxy_timeout</td><td>会话空闲超时（默认 10m）</td><td>10s</td></tr>
      <tr><td>proxy_responses</td><td>UDP：每请求预期应答数，收满即回收会话</td><td>1</td></tr></table></details>

      <details><summary>常用变量</summary>
      <table><tr><th>变量</th><th>含义</th><th></th></tr>
      <tr><td>$uri / $request_uri</td><td>当前路径（可被 rewrite 改）/ 原始完整 URI</td><td></td></tr>
      <tr><td>$args / $is_args</td><td>查询串 / 有查询串时为 "?"</td><td></td></tr>
      <tr><td>$host / $scheme</td><td>请求 Host（去端口）/ http 或 https</td><td></td></tr>
      <tr><td>$remote_addr</td><td>客户端 IP（$binary_remote_addr 同义）</td><td></td></tr>
      <tr><td>$proxy_add_x_forwarded_for</td><td>追加客户端 IP 后的 XFF 链</td><td></td></tr>
      <tr><td>$http_*、$arg_*、$cookie_*</td><td>任意请求头 / 查询参数 / Cookie</td><td>$http_user_agent</td></tr>
      <tr><td>$upstream_addr</td><td>实际服务的后端地址（日志用）</td><td></td></tr>
      <tr><td>$request_time / $status …</td><td>log_formats 专用：耗时、状态码、字节数等</td><td>$body_bytes_sent</td></tr></table></details>
    </div>
    <div class="editor">
      <div class="gutter"><div id="lnums">1</div></div>
      <div class="edwrap">
        <pre id="hl" aria-hidden="true"></pre>
        <textarea id="conf" spellcheck="false" autocomplete="off" autocapitalize="off"
          oninput="edUpdate()" onscroll="edSync()" onkeydown="edKey(event)"></textarea>
      </div>
    </div>
    <div class="row btns" style="margin-top:10px">
      <button onclick="saveConf(false, this)">校验并保存</button>
      <button class="primary" onclick="saveConf(true, this)">保存并重载</button>
      <span class="msg" id="conf-msg"></span>
    </div>
  </div>
</div>

<script>
let curLog = 'success', alive = true;

function fmtUp(s) {
  const d = Math.floor(s/86400), h = Math.floor(s%86400/3600), m = Math.floor(s%3600/60);
  return (d? d+'天 ':'') + (h? h+'时 ':'') + m + '分';
}

let lastStatus = null;

async function refreshStatus() {
  try {
    const r = await fetch('/api/status');
    if (!r.ok) throw 0;
    const s = await r.json();
    alive = true;
    lastStatus = s;
    document.getElementById('dot').className = 'dot';
    document.getElementById('meta').textContent =
      'gonx/' + s.version + ' · pid ' + s.pid + ' · 运行 ' + fmtUp(s.uptime) +
      ' · HTTP ' + s.addrs.join(', ') + (s.streams.length ? ' · L4 ' + s.streams.join(', ') : '');
    const menu = document.getElementById('dd-menu');
    const want = s.addrs.join('|');
    if (menu.dataset.opts !== want) {   // 监听地址变化(如reload)时重建选项,保留选中值
      if (curPort && !s.addrs.includes(curPort)) curPort = '';
      const check = '<svg class="ck" viewBox="0 0 12 12" width="12" height="12"><path fill="none" ' +
        'stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" ' +
        'd="M2.5 6.2 5 8.7l4.5-5"/></svg>';
      const item = (val, label) => '<button data-port="' + val + '" onclick="pickPort(this)">' +
        check + '<span>' + label + '</span></button>';
      menu.innerHTML = item('', '全部端口') + s.addrs.map(a => item(a, a)).join('');
      menu.dataset.opts = want;
      syncPortUI();
    }
    renderStats();
  } catch (e) {
    alive = false;
    document.getElementById('dot').className = 'dot stopped';
    document.getElementById('meta').textContent = '服务已停止或不可达';
  }
}

let curPort = '';

function toggleDD(e) {
  e.stopPropagation();
  document.getElementById('portdd').classList.toggle('open');
  document.getElementById('dd-menu').hidden = !document.getElementById('portdd').classList.contains('open');
}

function closeDD() {
  document.getElementById('portdd').classList.remove('open');
  document.getElementById('dd-menu').hidden = true;
}

function pickPort(btn) {
  curPort = btn.dataset.port;
  closeDD();
  syncPortUI();
  renderStats();
}

// 同步下拉按钮文字与菜单项选中态
function syncPortUI() {
  document.getElementById('dd-label').textContent = curPort || '全部端口';
  document.querySelectorAll('#dd-menu button').forEach(b =>
    b.className = b.dataset.port === curPort ? 'on' : '');
}

document.addEventListener('click', e => {
  if (!e.target.closest('#portdd')) closeDD();
});

function renderStats() {
  if (!lastStatus) return;
  let s = lastStatus;
  if (curPort) {
    s = (lastStatus.byAddr || []).find(x => x.addr === curPort) || {total:0,c2xx:0,c3xx:0,c4xx:0,c5xx:0};
  }
  for (const k of ['total','2xx','3xx','4xx','5xx'])
    document.getElementById('t-'+k).textContent = s[k === 'total' ? 'total' : 'c'+k];
}

async function refreshLog(force) {
  if (!alive) return;
  try {
    const pre = document.getElementById('log');
    // 仅当用户本来就贴在底部时才跟随滚动，翻历史时不打断
    const stick = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 30;
    const r = await fetch('/api/logs?name=' + curLog);
    const txt = (await r.text()) || '(空)';
    pre.textContent = txt;
    document.getElementById('loglnums').textContent = lineNums(txt);
    if (force || stick) pre.scrollTop = pre.scrollHeight;
    logSync();
  } catch (e) {}
}

function lineNums(txt) {
  const n = txt.split('\n').length;
  const out = [];
  for (let i = 1; i <= n; i++) out.push(i);
  return out.join('\n');
}

function logSync() {
  const pre = document.getElementById('log');
  document.getElementById('loglnums').style.transform = 'translateY(' + (-pre.scrollTop) + 'px)';
}

async function clearLog(btn) {
  const name = curLog, m = document.getElementById('log-msg');
  if (!confirm('确定清空 ' + name + '.log？内容不可恢复。')) return;
  btn.disabled = true; m.textContent = '…'; m.className = 'msg';
  try {
    const r = await fetch('/api/logs/clear?name=' + encodeURIComponent(name), {method: 'POST'});
    m.textContent = (await r.text()).trim();
    m.className = 'msg ' + (r.ok ? 'ok' : 'err');
    if (r.ok) refreshLog(true);
  } catch (e) { m.textContent = '请求失败'; m.className = 'msg err'; }
  btn.disabled = false;
  setTimeout(() => { if (m.className !== 'msg err') m.textContent = ''; }, 4000);
}

function pickLog(btn) {
  document.querySelectorAll('.tabs button[data-log]').forEach(b => b.className = '');
  btn.className = 'on';
  curLog = btn.dataset.log;
  refreshLog(true);
}

async function act(path, btn) {
  const m = document.getElementById('act-msg');
  btn.disabled = true; m.textContent = '…'; m.className = 'msg';
  try {
    const r = await fetch(path, {method: 'POST'});
    const t = (await r.text()).trim();
    const lines = t.split('\n').filter(Boolean);
    if (r.ok && lines.length > 1) {
      // 首行是结果，其余是警告（如 ui 段变更需重启）→ 醒目提示
      m.textContent = lines[0] + '｜' + lines.slice(1).join(' ');
      m.className = 'msg warn';
    } else {
      m.textContent = t; m.className = 'msg ' + (r.ok ? 'ok' : 'err');
    }
  } catch (e) { m.textContent = '请求失败'; m.className = 'msg err'; }
  btn.disabled = false;
  refreshStatus();
}

async function stopServer(btn) {
  if (!confirm('确定停止 gonx 服务？停止后本页面将不可用。')) return;
  await act('/stop', btn);
}

async function loadConf() {
  const r = await fetch('/api/config');
  const c = await r.json();
  document.getElementById('conf-path').textContent = c.path;
  document.getElementById('conf').value = c.content;
  edUpdate();
}

// ---- VS Code 风格编辑器：高亮层 + 透明文本框叠加 ----

function escHTML(s) {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

// 按行做 YAML 高亮：键名、字符串、注释、数字/单位、布尔、$变量。
// 先用控制字符占位标记 token，最后统一替换成 <span>，避免后续正则
// 匹配到已生成的 HTML 标记本身（比如 class="tk" 里的引号）。
function hlYAML(src) {
  return escHTML(src).split('\n').map(function (line) {
    let keyed = line.replace(/^(\s*(?:-\s+)?)([A-Za-z_][\w.-]*)(:)(?=\s|$)/,
      function (m, p1, p2, p3) { return p1 + '\u0001' + p2 + '\u0007' + p3; });
    // 流式映射里的行内属性名：{from: x, to: y}
    keyed = keyed.replace(/([{,]\s*)([A-Za-z_][\w.-]*)(:)(?=\s)/g,
      function (m, p1, p2, p3) { return p1 + '\u0001' + p2 + '\u0007' + p3; });
    keyed = keyed.replace(
      /(#.*$)|('[^']*')|("[^"]*")|(\$\{?[A-Za-z_]\w*\}?)|(\b\d+(?:\.\d+)?(?:k|m|g|ms|s|h|d|r\/s|r\/m)?\b)|(\b(?:true|false|on|off|null)\b)|([{}\[\]])/g,
      function (m, c, s1, s2, v, n, b, p) {
        if (c) return '\u0002' + c + '\u0007';
        if (s1 || s2) return '\u0003' + m + '\u0007';
        if (v) return '\u0004' + m + '\u0007';
        if (n) return '\u0005' + m + '\u0007';
        if (b) return '\u0006' + m + '\u0007';
        return '\u0008' + m + '\u0007';
      });
    return keyed
      .replace(/\u0001/g, '<span class="tk">')
      .replace(/\u0002/g, '<span class="tc">')
      .replace(/\u0003/g, '<span class="ts">')
      .replace(/\u0004/g, '<span class="tv">')
      .replace(/\u0005/g, '<span class="tn">')
      .replace(/\u0006/g, '<span class="tb">')
      .replace(/\u0008/g, '<span class="tp">')
      .replace(/\u0007/g, '</span>');
  }).join('\n');
}

function edUpdate() {
  const ta = document.getElementById('conf');
  document.getElementById('hl').innerHTML = hlYAML(ta.value) + '\n';
  document.getElementById('lnums').textContent = lineNums(ta.value);
  edSync();
}

function edSync() {
  const ta = document.getElementById('conf');
  const hl = document.getElementById('hl');
  hl.scrollTop = ta.scrollTop;
  hl.scrollLeft = ta.scrollLeft;
  document.getElementById('lnums').style.transform = 'translateY(' + (-ta.scrollTop) + 'px)';
}

function edInsert(ta, text) {
  // execCommand 保留撤销历史；不可用时退回手动拼接
  if (!document.execCommand || !document.execCommand('insertText', false, text)) {
    const s = ta.selectionStart, e = ta.selectionEnd;
    ta.value = ta.value.slice(0, s) + text + ta.value.slice(e);
    ta.selectionStart = ta.selectionEnd = s + text.length;
  }
  edUpdate();
}

function edKey(e) {
  const ta = e.target;
  if (e.key === 'Tab') {
    e.preventDefault();
    edInsert(ta, '  ');
  } else if (e.key === 'Enter') {
    e.preventDefault();
    const v = ta.value, s = ta.selectionStart;
    const lineStart = v.lastIndexOf('\n', s - 1) + 1;
    const indent = (v.slice(lineStart, s).match(/^\s*/) || [''])[0];
    edInsert(ta, '\n' + indent);
  }
}

async function saveConf(reload, btn) {
  const m = document.getElementById('conf-msg');
  btn.disabled = true; m.textContent = '校验中…'; m.className = 'msg';
  try {
    const r = await fetch('/api/config', {method: 'POST', body: document.getElementById('conf').value});
    const t = await r.text();
    if (!r.ok) { m.textContent = t.trim(); m.className = 'msg err'; btn.disabled = false; return; }
    if (reload) {
      const r2 = await fetch('/reload', {method: 'POST'});
      const t2 = await r2.text();
      m.textContent = t.trim() + '；' + t2.trim();
      m.className = 'msg ' + (r2.ok ? 'ok' : 'err');
    } else {
      m.textContent = t.trim(); m.className = 'msg ok';
    }
  } catch (e) { m.textContent = '请求失败'; m.className = 'msg err'; }
  btn.disabled = false;
  refreshStatus();
}

function toggleHelp() {
  const h = document.getElementById('help');
  h.hidden = !h.hidden;
  if (!h.hidden) document.getElementById('help-q').focus();
}

function filterHelp() {
  const q = document.getElementById('help-q').value.trim().toLowerCase();
  document.querySelectorAll('#help details').forEach((d, idx) => {
    if (!q) {
      d.style.display = '';
      d.open = idx === 0; // 默认只展开配置示例
      d.querySelectorAll('tr').forEach(tr => tr.style.display = '');
      return;
    }
    const rows = d.querySelectorAll('tr');
    if (rows.length === 0) {
      // 无表格的分组（如配置示例）按整体文本匹配
      const hit = d.textContent.toLowerCase().includes(q);
      d.style.display = hit ? '' : 'none';
      d.open = hit;
      return;
    }
    let any = false;
    rows.forEach((tr, i) => {
      if (i === 0) return; // header row
      const hit = tr.textContent.toLowerCase().includes(q);
      tr.style.display = hit ? '' : 'none';
      if (hit) any = true;
    });
    d.style.display = any ? '' : 'none';
    d.open = any;
  });
}

refreshStatus(); refreshLog(true); loadConf();
setInterval(refreshStatus, 3000);
setInterval(() => { if (document.getElementById('auto').checked) refreshLog(); }, 3000);
</script>
</body>
</html>`
