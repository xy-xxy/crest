<table border="0" cellspacing="0" cellpadding="0"><tr>
<td width="220" align="center" valign="middle"><img src="crest.png" width="190" alt="crest"></td>
<td valign="middle"><h1>crest</h1><b>nginx-compatible web server &amp; reverse proxy, written in Go</b></td>
</tr></table>

![Go](https://img.shields.io/badge/Go-1.20%2B-00ADD8?logo=go&logoColor=white)
![Version](https://img.shields.io/badge/version-v1.0.0-17a2c6)
![Platform](https://img.shields.io/badge/platform-Windows%20%7C%20Linux%20(amd64%2Farm64)-555)
![Tests](https://img.shields.io/badge/tests-9%20packages%20passing-brightgreen)
![Config](https://img.shields.io/badge/config-nginx.conf%20%2B%20YAML-17a2c6)

> Release notes: [English](RELEASE_NOTES_EN.md) · [中文](RELEASE_NOTES_CN.md) — current version **v1.0.0**

crest is a single-binary, zero-dependency web server and reverse proxy. It runs
your existing `nginx.conf` as-is, adds a cleaner native YAML format, and ships a
built-in browser admin console — aimed at small / internal / self-hosted
deployments.

Two config formats, auto-detected by file extension:

- **`.yml` / `.yaml`** — crest's native YAML format (recommended, see `crest.yml`)
- **anything else (`.conf`, …)** — nginx.conf-compatible syntax (drop in your existing config)

Both parse to the exact same internal structure, so they are functionally equivalent.

> [!IMPORTANT]
> crest targets deployments where nginx.conf compatibility and a friendly admin UI
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
# no arguments: picks up crest.yml / crest.yaml / crest.conf from the working
# directory, then from beside the executable — this is the double-click case
./crest

# start with a YAML config
go run ./cmd/crest -c crest.yml

# start with an nginx.conf config
go run ./cmd/crest -c testdata/nginx_basic.conf

# validate a config (like nginx -t; works for both formats)
go run ./cmd/crest -t -c crest.yml

# build
go build -o crest.exe ./cmd/crest
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
A full runnable example lives at the repo root: `crest.yml` (a direct translation
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

By default (when the config sets no explicit `access_log` / `error_log`), crest
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
  split happens — except for `error_log: stderr` (and `/dev/stderr`), which is
  routed through the same tee as the default so runtime errors still land in
  `console.log`. Writing to a detached process's bare stderr would throw them
  away, which is exactly when you need them.
- The `listening on` line in `console.log` is info level and filtered out by the
  default `error` level; set `error_log.level` to `info` to see it.
- Pair with `crest -s reopen` for log rotation.

## Ops commands

Like `nginx -s`. `-c` may be omitted whenever the config is discoverable — the
same probe the no-argument start uses, so running these from the config's own
directory needs no path:

```sh
crest -s reload   # hot reload: switches only if the new config validates
crest -s reopen   # reopen log files (for external logrotate)
crest -s status   # show runtime status
crest -s stop     # graceful stop
crest -s ui       # open the browser admin console

crest -s reload -c /etc/crest/crest.yml   # or point at one explicitly
```

The control file is `<conf>.ctl`, so `-c` must name the *same* config the
running instance was started with.

The token-protected control channel exposes JSON `/healthz` and `/readyz`, plus
the compatible text `/status`. Send `Accept: application/json` or use
`/status?format=json` for structured status data.

## Admin console

The admin console **runs inside the crest server process** (it is the control
channel). Two ways to open it:

1. `crest -s ui -c <conf>` — open the browser any time (works with the default
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
channel — crest warns you if you reload after changing it). Keep `listen` on
`127.0.0.1`; binding a non-loopback address makes the console reachable from the
network — add an `allow` list, or crest warns.

Features:

- **Request stats**: total / 2xx / 3xx / 4xx / 5xx, live, with per-port filter
- **One-click actions**: reload config, reopen logs, graceful stop
- **Log viewer**: tail 200 lines of success.log / error.log / console.log,
  auto-refresh, line numbers, and a **clear** button for the log on screen
  (truncates in place, so the running server keeps writing to it)
- **In-browser config editor**: VS Code-style syntax highlighting; validated by
  the real parser before saving (a bad config is rejected with 400), auto-backup
  to `.bak`, one-click "save & reload"; a config help drawer with a full annotated example

The page is a single self-contained HTML file (no external assets), light/dark
theme aware, and works on Windows / Linux.

## Linux deployment

Pure Go, no cgo — cross-compile to a dependency-free static binary (runs on any
distro, or a `scratch` container):

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o crest ./cmd/crest
# arm64: GOARCH=arm64
```

Or build all targets at once with `./build.sh` (Windows with icon + Linux
amd64/arm64).

On Linux: binding ports 80/443 needs root or
`setcap 'cap_net_bind_service=+ep' crest`; log rotation works with standard
logrotate (`mv` + `crest -s reopen`) with none of Windows' file-locking limits.

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
- **connection acceptance**: each HTTP listener has a bounded 512-connection
  accept queue to absorb short bursts before HTTP/TLS initialization. This is
  separate from active-request limits and requires OS socket/file-descriptor
  headroom.
- **upstream connections**: defaults to 512 idle connections per backend host
  (explicit upstream `keepalive` overrides it). Each proxy transport admits at
  most 16 concurrent TCP connection attempts; waiting respects request
  cancellation and `proxy_connect_timeout`. Established streams are unaffected.
- **load balancing**: smooth weighted round-robin default; `least_conn` (by
  active/weight ratio); `ip_hash` (client sticks to one backend, fails over)
- **limit_req**: nginx's leaky-bucket (`rate=Nr/s|r/m`, `burst=`, `nodelay`),
  503 over the limit, queued requests released at the rate
- **limit_conn**: per-key concurrency count, 503 over the limit
- **proxy_cache**: disk store + in-memory index; TTL priority is upstream
  `Cache-Control`/`Expires` > `proxy_cache_valid`; `no-store`/`private`/
  `Set-Cookie`/`Authorization` requests are not cached; responses carry
  `X-Cache-Status: HIT|MISS|BYPASS` (`BYPASS` means the recording budget is full).
  Cold after restart (index not persisted), and
  `Vary` selects independent variants by request header. GET and HEAD share
  entries, only GET populates them, and `Vary: *` is never cached.
- **stream (L4)**: TCP and UDP proxying with upstream load balancing and
  failover; UDP keeps a session per client address, `proxy_responses` controls
  early session teardown
- **error_page**: intercepts crest-generated errors (404/502/503 …), internally
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

A buffered response uses a per-request memory budget from `proxy_buffers`;
the spooler allows up to twice that budget while handing data to disk, plus
in-flight chunks. Size this deliberately for concurrent large downloads, or
turn buffering off for that location. Cache misses can record at most four
responses across the process (8 MiB body limit each); additional misses stream
uncached. `no-store`, `Vary: *`, HEAD misses and SSE also stream directly.

`GOMEMLIMIT` is a soft Go runtime target, not a hard process RSS limit. Use
`limit_conn` to bound active requests when memory is tight; excess requests
receive 503. Idle sockets, TLS/gzip state, headers, the OS and disk space need
separate headroom. See [the reproducible performance report](docs/performance.md)
for tested workloads and the connection-burst limit; it is not a guarantee for
arbitrary concurrency, payloads or deployment hardware.

Single-process, multi-goroutine model — no master/worker. One port cannot mix ssl
and plain. Requests with a body are not retried on upstream failure (the body may
already be consumed). Single cached object capped at 8MB (larger ones pass
through uncached).

- **TODO**: FastCGI (for PHP setups), `proxy_cache` index persistence, TLS
  termination for stream

## Build

```sh
go build -o crest.exe ./cmd/crest    # cmd/crest/rsrc_windows.syso embeds the icon
./build.sh                          # Windows + Linux amd64/arm64 + Windows 7 amd64,
                                    # built with -trimpath so no absolute source
                                    # paths from the build machine get embedded

# regenerate the icon (ico for the exe + png for this README)
go run ./tools/genicon -o cmd/crest/crest.ico -syso cmd/crest/rsrc_windows.syso \
    -png crest.png -png-size 200
```

The Windows 7 target (`crest-win7-amd64.exe`) requires a separate Go 1.20.14
SDK. By default the script uses
`$HOME/.local/share/crest-build/go1.20.14/go`; set
`WIN7_GOROOT=/path/to/go1.20.14 ./build.sh` to use another installation.
The other three targets continue using `go` from PATH. No global SDK settings
are changed; a missing or incorrect Win7 SDK stops the script before building.
Set `OUTPUT_DIR=dist/release ./build.sh` to build all four targets without
overwriting the existing executables at the repository root.

## Layout

```
cmd/crest/            entry point (-c/-t/-v/-s, graceful shutdown), admin console
internal/config/     nginx.conf lexer/parser → AST → typed config; YAML loader
internal/server/     listen aggregation, vhost & location matching, access logs
internal/handler/    static files (root/alias/index), reverse proxy (with retry),
                     gzip, cache, response buffering (proxy_buffering)
internal/upstream/   load-balancing pools
internal/logging/    access log (custom formats), leveled error log
internal/{limiter,cache,auth,stream,vars}/  rate limiting, proxy cache, htpasswd, L4 proxy, variables
tools/genicon/       draws the icon and emits both crest.ico and the Windows
                     resource object (no external tool needed)
```

## Tests

```sh
go test ./...
```

Covers: config parsing (quotes/comments/error recovery), location & server_name
matching matrices, weighted round-robin distribution and eviction, config
auto-discovery, and end-to-end (static + proxy + URI replacement + upstream
failover + multiple vhosts + TLS/SNI + rate limiting + cache + admin API).

Regression tests guard the failure modes that were hardest to find: WebSocket
upgrades through every middleware combination and through an upstream pool,
`root`/`alias` path mapping against traversal, HEAD not poisoning `proxy_cache`,
query strings surviving an absolute `rewrite`, `error_page` starting from clean
headers, and `proxy_buffering` releasing the backend ahead of a slow client.
Run them with the race detector too — the buffering path is concurrent:

```sh
go test -race ./...
```
