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
				os.WriteFile(ru.confPath+".bak", old, 0644)
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
<style>
:root {
  --bg: #f6f7f9; --surface: #ffffff; --border: #e3e6ea;
  --ink: #1c2733; --ink-2: #5b6b7b; --ink-3: #8a97a5;
  --accent: #009639; --accent-ink: #ffffff;
  --danger: #c62828; --warn: #b26a00;
  --mono: ui-monospace, Consolas, "Courier New", monospace;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #14181d; --surface: #1d232b; --border: #2c353f;
    --ink: #e4e9ee; --ink-2: #9aa8b6; --ink-3: #6b7885;
    --accent: #23b35c; --danger: #ef5350; --warn: #ffb74d;
  }
}
* { box-sizing: border-box; margin: 0; }
body { background: var(--bg); color: var(--ink); font: 14px/1.6 "Segoe UI", system-ui, sans-serif; padding: 24px; }
.wrap { max-width: 980px; margin: 0 auto; display: grid; gap: 16px; }
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
pre { background: var(--bg); border: 1px solid var(--border); border-radius: 8px; padding: 12px;
  font: 12px/1.5 var(--mono); overflow: auto; max-height: 320px; white-space: pre-wrap; word-break: break-all; }
textarea { width: 100%; height: 320px; background: var(--bg); border: 1px solid var(--border);
  border-radius: 8px; color: var(--ink); font: 13px/1.5 var(--mono); padding: 12px; resize: vertical; }
.help-btn { float: right; font-size: 12px; padding: 3px 10px; }
.help { border: 1px solid var(--border); border-radius: 8px; padding: 12px; margin-bottom: 10px;
  max-height: 360px; overflow: auto; background: var(--bg); }
.help input { width: 100%; padding: 6px 10px; margin-bottom: 8px; border: 1px solid var(--border);
  border-radius: 6px; background: var(--surface); color: var(--ink); font-size: 13px; }
.help details { margin-bottom: 4px; }
.help summary { cursor: pointer; font-weight: 600; font-size: 13px; padding: 4px 0; }
.help table { width: 100%; border-collapse: collapse; font-size: 12px; margin: 6px 0 10px; }
.help th, .help td { text-align: left; padding: 4px 8px; border-bottom: 1px solid var(--border); vertical-align: top; }
.help th { color: var(--ink-3); font-weight: 600; }
.help td:first-child { font-family: var(--mono); white-space: nowrap; }
.help td:last-child { font-family: var(--mono); color: var(--ink-2); }
.help .note { font-size: 12px; color: var(--ink-2); margin: 4px 0 8px; }
.msg { font-size: 13px; margin-left: 8px; }
.msg.ok { color: var(--accent); } .msg.err { color: var(--danger); white-space: pre-wrap; }
label.auto { font-size: 13px; color: var(--ink-2); margin-left: auto; user-select: none; }
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
    <h2>请求统计（自本次启动）</h2>
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
      <label class="auto"><input type="checkbox" id="auto" checked> 自动刷新</label>
    </div>
    <pre id="log">载入中…</pre>
  </div>

  <div class="card">
    <h2>配置文件 <code id="conf-path"></code>
      <button class="help-btn" onclick="toggleHelp()">配置帮助</button></h2>
    <div id="help" class="help" hidden>
      <input id="help-q" placeholder="搜索配置项，如 proxy_pass、limit、ssl…" oninput="filterHelp()">
      <div class="note">单位：大小 <b>1k / 200m / 1g</b>；时间 <b>30s / 10m / 2h / 1d</b>；速率 <b>5r/s / 60r/m</b>。未知字段会在保存校验时直接报错。</div>

      <details open><summary>顶层结构</summary>
      <table><tr><th>字段</th><th>说明</th><th>示例</th></tr>
      <tr><td>ui</td><td>管理页面：固定端口/令牌/自动打开（改后需重启）</td><td>listen: 127.0.0.1:8899</td></tr>
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

      <details><summary>server（虚拟主机）</summary>
      <table><tr><th>字段</th><th>说明</th><th>示例</th></tr>
      <tr><td>listen</td><td>监听端口；标量或带 ssl/default 的映射</td><td>[8080, {address: 443, ssl: true, default: true}]</td></tr>
      <tr><td>server_name</td><td>按 Host 匹配：精确 / *.foo / foo.* / ~正则</td><td>[example.com, "*.example.org"]</td></tr>
      <tr><td>root / index</td><td>静态根目录与默认页</td><td>root: dist</td></tr>
      <tr><td>ssl_certificate(_key)</td><td>PEM 证书 + 私钥；多 server 自动按 SNI 选择；TLS 下自动启用 HTTP/2</td><td>ssl_certificate: cert.pem</td></tr>
      <tr><td>error_pages</td><td>自定义错误页（仅拦截 gonx 自身错误，上游错误透传）</td><td>[{codes: [502,503], uri: /50x.html}]</td></tr>
      <tr><td>rewrites</td><td>server 级重写，location 匹配前执行</td><td>见 location</td></tr>
      <tr><td>locations</td><td>路由规则列表</td><td></td></tr></table></details>

      <details><summary>location（路由）</summary>
      <table><tr><th>字段</th><th>说明</th><th>示例</th></tr>
      <tr><td>path + match</td><td>匹配方式：prefix(默认) / exact / regex / regex_i / prefix_priority（对应 nginx 空 / = / ~ / ~* / ^~）</td><td>{path: /ping, match: exact}</td></tr>
      <tr><td>path: "@名字"</td><td>命名 location，仅由 try_files 跳转进入</td><td>try_files: [$uri, "@backend"]</td></tr>
      <tr><td>proxy_pass</td><td>反向代理；host 为 upstream 名或地址；带尾部 / 时替换匹配前缀；WebSocket 自动升级</td><td>http://pool/</td></tr>
      <tr><td>proxy_set_header</td><td>传给后端的请求头（支持变量）</td><td>{Host: $host, X-Real-IP: $remote_addr}</td></tr>
      <tr><td>proxy_cookie_path</td><td>改写后端 Set-Cookie 的 Path 前缀</td><td>[{from: /api/, to: /}]</td></tr>
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
      <tr><td>proxy_connect_timeout / proxy_read_timeout</td><td>连接/读后端超时</td><td>proxy_read_timeout: 300s</td></tr>
      <tr><td>gzip</td><td>同 http 层，可局部覆盖</td><td>{enabled: false}</td></tr></table></details>

      <details><summary>upstream（负载均衡）</summary>
      <table><tr><th>字段</th><th>说明</th><th>示例</th></tr>
      <tr><td>balancer</td><td>round_robin(默认加权轮询) / least_conn / ip_hash</td><td>balancer: least_conn</td></tr>
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
    <textarea id="conf" spellcheck="false"></textarea>
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

async function refreshStatus() {
  try {
    const r = await fetch('/api/status');
    if (!r.ok) throw 0;
    const s = await r.json();
    alive = true;
    document.getElementById('dot').className = 'dot';
    document.getElementById('meta').textContent =
      'gonx/' + s.version + ' · pid ' + s.pid + ' · 运行 ' + fmtUp(s.uptime) +
      ' · HTTP ' + s.addrs.join(', ') + (s.streams.length ? ' · L4 ' + s.streams.join(', ') : '');
    for (const k of ['total','2xx','3xx','4xx','5xx'])
      document.getElementById('t-'+k).textContent = s[k === 'total' ? 'total' : 'c'+k];
  } catch (e) {
    alive = false;
    document.getElementById('dot').className = 'dot stopped';
    document.getElementById('meta').textContent = '服务已停止或不可达';
  }
}

async function refreshLog(force) {
  if (!alive) return;
  try {
    const pre = document.getElementById('log');
    // 仅当用户本来就贴在底部时才跟随滚动，翻历史时不打断
    const stick = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 30;
    const r = await fetch('/api/logs?name=' + curLog);
    pre.textContent = (await r.text()) || '(空)';
    if (force || stick) pre.scrollTop = pre.scrollHeight;
  } catch (e) {}
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
    const t = await r.text();
    m.textContent = t.trim(); m.className = 'msg ' + (r.ok ? 'ok' : 'err');
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
  document.querySelectorAll('#help details').forEach(d => {
    if (!q) {
      d.style.display = '';
      d.open = d.querySelector('summary').textContent === '顶层结构';
      d.querySelectorAll('tr').forEach(tr => tr.style.display = '');
      return;
    }
    const rows = d.querySelectorAll('tr');
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
