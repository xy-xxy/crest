<table border="0" cellspacing="0" cellpadding="0"><tr>
<td width="220" align="center" valign="middle"><img src="gonx.png" width="190" alt="gonx"></td>
<td valign="middle"><h1>gonx</h1><b>nginx-compatible web server &amp; reverse proxy, written in Go</b></td>
</tr></table>

![Go](https://img.shields.io/badge/Go-1.20%2B-00ADD8?logo=go&logoColor=white)
![Version](https://img.shields.io/badge/version-v0.5.0-009639)
![Platform](https://img.shields.io/badge/platform-Windows%20%7C%20Linux%20(amd64%2Farm64)-555)
![Tests](https://img.shields.io/badge/tests-8%20packages%20passing-brightgreen)
![Config](https://img.shields.io/badge/config-nginx.conf%20%2B%20YAML-009639)

> Release notes: [English](RELEASE_NOTES_EN.md) · [中文](RELEASE_NOTES_CN.md) — current version **v0.5.0**

gonx is a single-binary, zero-dependency web server and reverse proxy. It runs
your existing `nginx.conf` as-is, adds a cleaner native YAML format, and ships a
built-in browser admin console — aimed at small / internal / self-hosted
deployments.

Two config formats, auto-detected by file extension:

- **`.yml` / `.yaml`** — gonx's native YAML format (recommended, see `gonx.yml`)
- **anything else (`.conf`, …)** — nginx.conf-compatible syntax (drop in your existing config)

Both parse to the exact same internal structure, so they are functionally equivalent.

> [!IMPORTANT]
> gonx targets deployments where nginx.conf compatibility and a friendly admin UI
> matter more than web-scale hardening. It is a single-author project with **no
> automatic HTTPS (ACME) and no service discovery** yet — for those, use nginx,
> Caddy or Traefik. See [Limitations and roadmap](#limitations-and-roadmap).

## Table of contents

- [Quick start](#quick-start)
- [YAML config at a glance](#yaml-config-at-a-glance)
- [Supported directives](#supported-directives)
- [Runtime logs](#runtime-logs)
- [Ops commands](#ops-commands)
- [Admin console](#admin-console)
- [Linux deployment](#linux-deployment)
- [nginx-aligned semantics](#nginx-aligned-semantics)
- [Limitations and roadmap](#limitations-and-roadmap)
- [Build](#build)
- [Layout](#layout)
- [Tests](#tests)

## Quick start

```sh
# start with a YAML config
go run ./cmd/gonx -c gonx.yml

# start with an nginx.conf config
go run ./cmd/gonx -c testdata/nginx_basic.conf

# validate a config (like nginx -t; works for both formats)
go run ./cmd/gonx -t -c gonx.yml

# build
go build -o gonx.exe ./cmd/gonx
```

## YAML config at a glance

```yaml
error_log: {path: logs/error.log, level: warn}

http:
  gzip: {enabled: true, min_length: 1k, types: [text/css, application/javascript]}

  upstreams:
    backend:
      balancer: least_conn        # round_robin | least_conn | ip_hash
      servers:
        - {address: 127.0.0.1:9001, weight: 3, max_fails: 2, fail_timeout: 30s}
        - {address: 127.0.0.1:9002, backup: true}

  servers:
    - listen: [8080, {port: 443, ssl: true, default: true}]
      server_name: [example.com, "*.example.org"]
      root: /var/www
      ssl_certificate: /etc/cert.pem
      ssl_certificate_key: /etc/key.pem
      error_pages:
        - {codes: [502, 503], uri: /50x.html}
      locations:
        - path: /
          try_files: [$uri, $uri/, /index.html]
        - path: /ping
          match: exact               # exact | prefix | regex | regex_i | prefix_priority
          return: {code: 200, body: pong}
        - path: /api/
          proxy_pass: http://backend
          proxy_set_header: {Host: $host, X-Real-IP: $remote_addr}
          proxy_cookie_path: [{from: /api/, to: /}]
          limit_req: {zone: perip, burst: 10, nodelay: true}
```

Unknown fields are rejected (`KnownFields`) so typos never slip through silently.
A full runnable example lives at the repo root: `gonx.yml` (a direct translation
of `nginx.conf`).

After starting, visit <http://localhost:8080/> (static page) and
<http://localhost:8080/ping> (the `return` directive).

## Supported directives

| Context | Directives |
|---|---|
| main | `http` `stream` (`events` / `worker_processes` etc. are accepted but ignored) |
| http | `server` `upstream` `log_format` `access_log` `error_log` `root` `index` `gzip*` `limit_req_zone` `limit_conn_zone` `proxy_cache_path` `client_max_body_size` `server_tokens` `ssl_protocols` `expires` `proxy_buffering` `proxy_buffers` `proxy_max_temp_file_size` |
| server | `listen` (with `ssl` / `default`) `server_name` `root` `index` `access_log` `location` `rewrite` `ssl_certificate` `ssl_certificate_key` `gzip*` `limit_req` `limit_conn` `proxy_cache` `proxy_cache_valid` `error_page` `client_max_body_size` `server_tokens` `ssl_protocols` `expires` `proxy_buffering` `proxy_buffers` `proxy_max_temp_file_size` |
| location | `root` `alias` `index` `proxy_pass` `return` `rewrite` `try_files` (incl. `@named`) `gzip*` `limit_req` `limit_conn` `proxy_cache` `proxy_cache_valid` `proxy_set_header` `proxy_cookie_path` `proxy_redirect` `proxy_hide_header` `proxy_pass_header` `client_max_body_size` `add_header` `allow`/`deny` `auth_basic`/`auth_basic_user_file` `autoindex` `expires` `proxy_connect_timeout`/`proxy_read_timeout` `proxy_buffering`/`proxy_buffers`/`proxy_max_temp_file_size` |
| upstream | `server` (with `weight=` `max_fails=` `fail_timeout=` `backup` `down`) `least_conn` `ip_hash` |
| stream | `upstream` `server` |
| stream server | `listen` (with `udp`) `proxy_pass` `proxy_timeout` `proxy_connect_timeout` `proxy_responses` |

`http` / `server` also accept `add_header` `allow`/`deny` `auth_basic*`
`autoindex` `map` and the timeout directives (nearest scope wins).

`gzip*` = `gzip` `gzip_types` `gzip_min_length` `gzip_comp_level` (inherited
across http/server/location).

## Runtime logs

By default (when the config sets no explicit `access_log` / `error_log`), gonx
splits runtime logs three ways into a `log/` directory **next to the executable**:

| File | Contents |
|---|---|
| `log/success.log` | Successful requests (HTTP status < 400) |
| `log/error.log` | Failed requests (status ≥ 400) |
| `log/console.log` | Process messages (start / reload / stop) and runtime errors, also echoed to the terminal |

Note: `error.log` here records **failed HTTP requests**; process-level errors
(nginx's `error_log`) go to `console.log`.

- The directory is based on the **executable's location**, not the working
  directory — `log/` is created wherever the binary runs.
- If the config sets `access_log` / `error_log` explicitly, that wins and no
  split happens.
- The `listening on` line in `console.log` is info level and filtered out by the
  default `error` level; set `error_log.level` to `info` to see it.
- Pair with `gonx -s reopen` for log rotation.

## Ops commands

Like `nginx -s`:

```sh
gonx -s reload -c <conf>   # hot reload: switches only if the new config validates
gonx -s reopen -c <conf>   # reopen log files (for external logrotate)
gonx -s status -c <conf>   # show runtime status
gonx -s stop   -c <conf>   # graceful stop
gonx -s ui     -c <conf>   # open the browser admin console
```

## Admin console

The admin console **runs inside the gonx server process** (it is the control
channel). Two ways to open it:

1. `gonx -s ui -c <conf>` — open the browser any time (works with the default
   random port + random token)
2. Pin the address in the config so it is ready at startup (bookmarkable,
   optional auto-open):

```yaml
ui:
  listen: 127.0.0.1:8899   # fixed admin port (random by default)
  token: my-secret          # fixed access token (random per start by default)
  open: true                # auto-open the browser on startup
  allow: [127.0.0.1, 192.168.0.0/16]  # IP allow list; only needed when bound to 0.0.0.0
```

The admin URL is printed to the terminal and `console.log` at startup. Changes to
the `ui` section require a full restart (reload does not rebuild the control
channel — gonx warns you if you reload after changing it). Keep `listen` on
`127.0.0.1`; binding a non-loopback address makes the console reachable from the
network — add an `allow` list, or gonx warns.

Features:

- **Request stats**: total / 2xx / 3xx / 4xx / 5xx, live, with per-port filter
- **One-click actions**: reload config, reopen logs, graceful stop
- **Log viewer**: tail 200 lines of success.log / error.log / console.log, auto-refresh, with line numbers
- **In-browser config editor**: VS Code-style syntax highlighting; validated by
  the real parser before saving (a bad config is rejected with 400), auto-backup
  to `.bak`, one-click "save & reload"; a config help drawer with a full annotated example

The page is a single self-contained HTML file (no external assets), light/dark
theme aware, and works on Windows / Linux.

## Linux deployment

Pure Go, no cgo — cross-compile to a dependency-free static binary (runs on any
distro, or a `scratch` container):

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o gonx ./cmd/gonx
# arm64: GOARCH=arm64
```

Or build all targets at once with `./build.sh` (Windows with icon + Linux
amd64/arm64).

On Linux: binding ports 80/443 needs root or
`setcap 'cap_net_bind_service=+ep' gonx`; log rotation works with standard
logrotate (`mv` + `gonx -s reopen`) with none of Windows' file-locking limits.

The control channel: the main process opens a token-guarded port on 127.0.0.1 and
writes its address to `<conf>.ctl` (mode 0600), which the `-s` commands read —
Windows has no SIGHUP, so this approach is consistent across platforms. Note that
reload is "build the new instance, close old listeners, start new ones", so the
port handoff has a millisecond window — not nginx's zero-downtime fd passing.

## nginx-aligned semantics

- **location matching**: `=` exact → `^~` prefix short-circuit → `~`/`~*` regex
  in config order → longest plain prefix
- **server_name matching**: exact → `*.example.com` (longest) → `www.*` (longest)
  → `~regex` → default server
- **root vs alias**: `root` appends the full URI, `alias` replaces the location prefix
- **proxy_pass**: with a URI it replaces the location prefix (`/api/x` +
  `http://b/v1/` → `/v1/x`); without one it passes the original URI through;
  default `Host: $proxy_host`, and injects `X-Real-IP` / `X-Forwarded-For` /
  `X-Forwarded-Proto`
- **proxy_buffering**: on by default, like nginx — the upstream response is
  read at full speed into a buffer (memory up to `proxy_buffers` count × size,
  then a temp file capped by `proxy_max_temp_file_size`) so the backend
  connection is released without waiting for a slow client. Two deliberate
  differences: the buffer is forwarded as soon as bytes arrive rather than
  once a buffer fills, so SSE and other incremental responses keep their
  latency with buffering left on; and the temp file is written on its own
  goroutine, keeping disk latency off the upstream's critical path. Set
  `proxy_buffering off` to stream straight through, or
  `proxy_max_temp_file_size 0` to keep the buffer in memory only
- **upstream**: smooth weighted round-robin (nginx's algorithm); connection
  failure fails over to the next backend and takes the failed one out for 10s
  (default `fail_timeout`)
- **access_log**: built-in `combined` format; custom `log_format` (supports
  `$remote_addr` `$request` `$status` `$body_bytes_sent` `$http_referer`
  `$http_user_agent` `$host` `$request_time` `$upstream_addr` …); path can be
  `stdout` / `stderr` / a file
- **variables**: `$uri` `$request_uri` `$args` `$is_args` `$host` `$scheme`
  `$remote_addr` `$request_method` `$http_*` `$arg_*` `$cookie_*` etc. in
  `return`/`rewrite`/`try_files`; `${name}` form also works
- **rewrite**: `last`/`break`/`redirect`/`permanent` flags, `$1`-`$9` captures,
  the `?` query-merge / trailing-`?` drop behavior matches nginx; `last` re-runs
  location matching (bounded to 10 cycles)
- **try_files**: `try_files $uri $uri/ /fallback;`, last item is an internal
  redirect or a `=404`-style status — ready for SPAs
- **HTTPS**: `listen 443 ssl` + `ssl_certificate`/`ssl_certificate_key`, certs
  chosen by SNI on `server_name`; the standard library negotiates **HTTP/2**
  automatically. TLS 1.2 minimum by default (`ssl_protocols` to override)
- **gzip**: filtered by Content-Type (`text/html` always), `gzip_min_length`
  threshold, 206 / already-encoded responses skipped
- **load balancing**: smooth weighted round-robin default; `least_conn` (by
  active/weight ratio); `ip_hash` (client sticks to one backend, fails over)
- **limit_req**: nginx's leaky-bucket (`rate=Nr/s|r/m`, `burst=`, `nodelay`),
  503 over the limit, queued requests released at the rate
- **limit_conn**: per-key concurrency count, 503 over the limit
- **proxy_cache**: disk store + in-memory index; TTL priority is upstream
  `Cache-Control`/`Expires` > `proxy_cache_valid`; `no-store`/`private`/
  `Set-Cookie`/`Authorization` requests are not cached; responses carry
  `X-Cache-Status: HIT|MISS`. Cold after restart (index not persisted), `Vary`
  not yet handled
- **stream (L4)**: TCP and UDP proxying with upstream load balancing and
  failover; UDP keeps a session per client address, `proxy_responses` controls
  early session teardown
- **error_page**: intercepts gonx-generated errors (404/502/503 …), internally
  redirects to a custom page keeping the original status; upstream errors pass
  through (matching `proxy_intercept_errors off`, so a backend's JSON error is
  never replaced with HTML)
- **client_max_body_size**: 413 over the limit, enforced while reading chunked bodies
- **add_header**: supports `always`; nginx nearest-scope override (a location's
  own headers shadow server/http)
- **allow / deny**: IP / CIDR / `all` access control, first match wins
- **auth_basic**: HTTP Basic auth, htpasswd supports bcrypt / Apache-MD5
  (`$apr1$`) / SHA1 / plaintext
- **autoindex**: directory listing (nginx-style HTML)
- **named location `@name`**: with `try_files $uri @backend` for "static first,
  fall back to proxy"
- **map + `$upstream_addr`**: variable system extended with mapped variables and
  the served backend address, usable in `log_format`
- **upstream health**: `max_fails` / `fail_timeout` sliding-window failure count,
  `backup` nodes, `down` manual removal, counter reset on success
- **proxy_redirect / proxy_hide_header / proxy_pass_header**: rewrite the upstream
  `Location` header and control which response headers pass through
- **expires**: `Cache-Control` + `Expires` headers (duration / `off` / `epoch` /
  `max` / negative no-cache)
- **server_tokens**: whether the `Server` header exposes the version
- Unknown directives are warned and ignored, so a real nginx.conf mostly runs
  as-is

## Limitations and roadmap

A buffered response holds up to `proxy_buffers` bytes of memory per in-flight
request, so a path serving large downloads to many clients at once should size
it deliberately (or turn buffering off for that location).

Single-process, multi-goroutine model — no master/worker. One port cannot mix ssl
and plain. Requests with a body are not retried on upstream failure (the body may
already be consumed). Single cached object capped at 8MB (larger ones pass
through uncached).

- **TODO**: FastCGI (for PHP setups), `proxy_cache` `Vary` support and index
  persistence, TLS termination for stream

## Build

```sh
go build -o gonx.exe ./cmd/gonx    # cmd/gonx/rsrc_windows.syso embeds the icon
./build.sh                          # all targets: Windows + Linux amd64/arm64

# regenerate the icon (ico for the exe + png for this README)
go run ./tools/genicon -o cmd/gonx/gonx.ico -png gonx.png -png-size 200
```

## Layout

```
cmd/gonx/            entry point (-c/-t/-v/-s, graceful shutdown), admin console
internal/config/     nginx.conf lexer/parser → AST → typed config; YAML loader
internal/server/     listen aggregation, vhost & location matching, access logs
internal/handler/    static files (root/alias/index), reverse proxy (with retry), gzip, cache
internal/upstream/   load-balancing pools
internal/logging/    access log (custom formats), leveled error log
internal/{limiter,cache,auth,stream,vars}/  rate limiting, proxy cache, htpasswd, L4 proxy, variables
```

## Tests

```sh
go test ./...
```

Covers: config parsing (quotes/comments/error recovery), location & server_name
matching matrices, weighted round-robin distribution and eviction, and end-to-end
(static + proxy + URI replacement + upstream failover + multiple vhosts + TLS/SNI
+ rate limiting + cache + admin API).
