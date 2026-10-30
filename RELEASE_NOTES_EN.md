# crest

A drop-in nginx alternative written in Go: single binary, zero runtime
dependencies, natively compatible with `nginx.conf`, plus a cleaner YAML config
format and a built-in browser admin console.

## v1.0.0 — 2026-10-30

- Reduced connection churn under burst proxy traffic: the default upstream idle pool increases from
  100 to 512 connections per host; each proxy transport limits concurrent TCP
  dials to 16, with cancellation and connect-timeout support. Active streaming
  connections are not capped by this dial budget. Explicit `keepalive` remains
  authoritative.
- Added a bounded TCP accept queue (512 pending connections per HTTP listener)
  so connection acceptance is decoupled from HTTP/TLS initialization. Shutdown
  closes pending sockets and leaves active requests to normal graceful shutdown.
- Bounded cache response recording to four concurrent objects across the process
  (8 MiB body limit each). Excess misses stream uncached with `X-Cache-Status:
  BYPASS`. Ineligible responses, SSE and HEAD misses stream without whole-body
  buffering; HEAD misses preserve their status and headers.
- Cached files now use immutable generations. Concurrent replacement cannot mix
  old headers with a new body, and files are retired after their readers close.
  Graceful shutdown cleans up this instance's cache files. Multi-value Vary
  request headers and equivalent Vary ordering are handled consistently.
- Removed a busy loop in the proxy buffer while waiting for the next body chunk.
  Cache publication still precedes release of a cacheable response to the client.
- Added a reproducible concurrency/RSS harness, regression tests and a detailed
  [performance report](docs/performance.md), including pre-fix failures.
- Added a separate Go 1.20.14 Windows 7 build target and `OUTPUT_DIR` support for
  packaging without replacing existing executables or the host SDK.

These are measured workload limits, not an unlimited-concurrency or fixed-RSS
promise. `GOMEMLIMIT` is soft; configure admission limits and leave OS headroom.
Known limits: macOS cold-start loopback bursts can still reset connections;
Linux repeat-run P99 latency reached 4.2s, so this is not a low-latency SLO
guarantee. Windows/Win7/Linux artifacts are cross-compiled; performance is checked on
macOS ARM64 and an isolated Linux ARM64 VM. Win7 uses a legacy Go toolchain. Cache index persistence, FastCGI
and stream TLS termination remain future work.

## v0.7.0

Added and fixed:

- Added token-protected JSON `/healthz` and `/readyz`. `/readyz` reports ready
  only after an HTTP or stream listener binds successfully, and becomes
  unavailable as shutdown begins.
- Added JSON status output through `Accept: application/json` or
  `/status?format=json`, while keeping the compatible one-line text `/status`.
- Added production `proxy_cache` Vary variants with constant-time lookup;
  `Vary: *` responses are skipped, GET populates shared entries, and HEAD can
  read them without poisoning the body.
- Restored the original request Host and URL after proxy handling so reusing a
  request object cannot change its cache key. Cacheable responses are inserted
  before their buffered body is released to the client, preventing an
  immediate follow-up request from racing the cache write.

The cache index is still in-memory and cold after restart. FastCGI and TLS
termination for stream remain future work.

## v0.6.0

**Renamed: gonx → crest.** The old name carried nginx's shadow, and this project long outgrew "nginx, but in Go". A crest is the lip of a weir — the line water passes over, which is exactly where traffic gets routed, split and throttled.

Upgrading needs manual steps; every one of these identifiers changed:

| Item | Old | New |
|---|---|---|
| Executable | `gonx.exe` / `gonx-linux-*` | `crest.exe` / `crest-linux-*` |
| Default config file | `gonx.conf` | `crest.conf` |
| Control file | `<conf>.ctl` | unchanged |
| Admin token header | `X-Gonx-Token` | `X-Crest-Token` |
| Admin console cookie | `gonx_token` | `crest_token` |
| pprof env var | `GONX_PPROF` | `CREST_PPROF` |
| `Server` response header | `gonx/<version>` | `crest/<version>` |
| Go module path | `gonx` | `crest` |

**Config file contents need no changes** — directive syntax and YAML field names are identical. Rename `gonx.yml` to `crest.yml` (or keep any name and pass `-c`). Ops commands go from `gonx -s reload` to `crest -s reload`.

**The config file is auto-detected when `-c` is omitted.** The default used to be a fixed `crest.conf`, while the recommended format is YAML — so double-clicking the exe on Windows usually failed outright, with the console vanishing before the message could be read. Omitting `-c` now probes `crest.yml` / `crest.yaml` / `crest.conf` in the working directory and then beside the executable, and `-s reload` and friends resolve the same way. When nothing is found the error lists every path tried instead of just saying no config exists, and it is written to `log/console.log` as well, so it survives the window closing. An explicit `-c` behaves exactly as before.

New icon too: the old one was nginx green with a white letter G — both parts of the old identity. The new mark is a deep-blue-to-cyan gradient behind two white water lines (a cresting wave over its wake). `genicon` now also emits the Windows resource object `rsrc_windows.syso` directly, which previously needed an external tool like rsrc or goversioninfo, so the exe icon rebuilds with the project.

Release builds now pass `-trimpath`, so binaries no longer embed the build machine's absolute source paths (which previously leaked the builder's username and directory layout).

## v0.5.0

Added:

- **`proxy_buffering` / `proxy_buffers` / `proxy_max_temp_file_size`** — the
  last substantial mechanism crest was missing next to nginx: buffering the
  upstream response so a slow client no longer holds the backend connection
  open. crest streamed straight through before, so a slow download pinned the
  upstream for its whole duration (measured: a 4MB response to a 12MB/s client
  held the backend ~150ms); with buffering the same case releases it in
  **1–9ms**. On by default, matching nginx, inherited http < server < location.

  Two deliberate differences from nginx: **(1)** buffered bytes are forwarded
  as soon as they arrive rather than once a buffer fills, so SSE and other
  incremental responses keep their latency with buffering left on — in nginx
  those need it switched off; **(2)** the temp file is written on its own
  goroutine, keeping disk latency off the upstream's critical path. An earlier
  inline-spool version measured no better than not buffering at all (50–183ms)
  before the write moved off that path.

  `proxy_buffers: {count, size}` sets the per-response memory budget (default
  8×32k) beyond which data spools to disk; `proxy_max_temp_file_size: 0` keeps
  the buffer in memory only, falling back to back-pressure past it; and
  `proxy_buffering: false` streams straight through. `proxy_buffer_size`,
  `proxy_busy_buffers_size`, `proxy_request_buffering` and
  `proxy_temp_file_write_size` have no counterpart in crest and are now
  accepted silently instead of warned about.

## v0.4.7

Added:

- **A "clear" button in the admin console's log panel** — empties the log
  currently on screen (`success.log` / `error.log` / `console.log`) behind a
  confirmation. The writers hold the file in `O_APPEND` mode, so it truncates
  in place: no reopen, no sparse gap. Backed by
  `POST /api/logs/clear?name=<success|error|console>`, guarded by the same
  token and `ui.allow` list as the rest of the console.

Security fixes:

- **The `.bak` the admin console writes when saving a config was
  world-readable.** The backup is a full copy of the config, which normally
  carries `ui.token`. The control file `.ctl` has always been written 0600,
  but the backup was hard-coded to 0644, so any local user could read the
  admin token out of `crest.yml.bak`. The backup now inherits the original
  config file's permission bits, falling back to 0600.
- **`..\` escaped the configured root on Windows.** A backslash in the URI is
  an ordinary character to `path.Clean`, but Windows `filepath.Join` treats it
  as a real separator and resolves the `..`, so a request like
  `/..%5C..%5Ccrest.yml` could read files outside the root — including the
  config file holding the admin token. Linux was unaffected (a backslash is a
  legal filename character there). The mapping now normalises backslashes to
  URI separators before cleaning and re-checks that the result is still inside
  the base directory; both guards apply on every platform. Browsers already
  rewrite `\` to `/` per the WHATWG URL spec, so normal traffic is unchanged.

Fixes:

- **WebSockets through an `upstream` pool always returned 502.** When the
  target is an upstream, the response body is wrapped so the backend returns
  to the pool once the connection closes (least_conn's active count depends on
  it) — but the wrapper exposed only `io.ReadCloser`, dropping `Write`. Go's
  reverse proxy type-asserts a 101 response body to `io.ReadWriteCloser` to
  build the bidirectional tunnel, so the assertion failed with `101 switching
  protocols response with non-writable body` and the request became a 502. A
  direct `proxy_pass http://host:port` was unaffected; only
  `proxy_pass http://<upstream>` triggered it. The wrapper now forwards
  `Write`, keeping the release behaviour intact.
- **`error_page` handed the intercepted response's headers to the error
  page.** The 502's own `Content-Type: text/plain` overrode the error page's
  real type, so a custom HTML page **rendered as source** in the browser, and
  the internal interception marker `X-Crest-Internal` leaked to the client. The
  staged headers are now cleared on interception, so the error page starts as
  a fresh response the way nginx builds it.
- **A server with only proxy locations and no `root` answered 500 instead of
  404 for unmatched requests.** That shape is common (a port hosting nothing
  but a few `proxy_pass` locations), and every stray `/favicon.ico` or scanner
  probe of `/` was recorded as a server error — unlike nginx, which falls back
  to its built-in default root and 404s, and noisy enough to pollute
  `error.log` and monitoring. It now returns 404 and logs one warn-level line
  (filtered out at the default error level).
- **A HEAD request poisoned `proxy_cache`.** HEAD shares the cache key with
  GET but carries no body, so its empty response was stored and every later
  GET answered `200` with an **empty body**. Only a GET may populate an entry
  now; HEAD still reads from one a GET created.
- **`rewrite` to an absolute URL dropped the original query string.** nginx
  appends the previous request arguments to the replacement unless it ends in
  `?`; crest did that only for local replacements and redirected to an absolute
  URL verbatim, so `rewrite ^/x/(.*)$ http://host/$1 redirect;` lost the
  client's `?a=1` entirely. A trailing `?` no longer leaves a bare `?` dangling
  in the `Location` header either.
- **WebSockets always returned 502 on any route with `add_header` or `expires`.**
  The response writers for those two directives (`headerAdder` / `expiresWriter`)
  did not forward `Hijack()`, so the reverse proxy could not take over the
  underlying connection to complete the 101 upgrade; Go's proxy failed with
  `can't switch protocols using non-Hijacker ResponseWriter type` and returned
  `502 Bad Gateway`. Because `add_header` / `expires` are inherited from the
  http / server scope, a single outer directive broke every WebSocket under that
  server while the location itself looked perfectly fine — a hard failure to
  track down. Both writers now pass Hijack through, matching `gzip` /
  `proxy_cache` / `limit_rate` and the other wrappers.
- **`error_log: {path: stderr}` silently swallowed every runtime error.**
  Errors only reached `log/console.log` when `error_log` was unset; an explicit
  `stderr` wrote to the process's raw stderr instead — which nobody reads when
  crest runs as a Windows service, a double-clicked exe, or any detached process,
  so diagnostics like `proxy error for ...` were lost and a 502 left nothing but
  a bare status code in the access log. `stderr` (and `/dev/stderr`) now go
  through the console tee like the unset case, reaching both the terminal and
  `log/console.log`, matching the documented role of console.log as the home for
  process messages and runtime errors.
- Added a WebSocket upgrade regression test covering a bare proxy plus the
  `add_header` / `expires` / `gzip` / `error_page` combinations.

## v0.4.6

- `rewrites` — now accepts both a single-rule mapping `rewrites: {pattern: ..., replacement: ..., flag: redirect}` and a multi-rule list `rewrites: [{...}, {...}]`. Previously only a list was accepted, so a single rule still had to be written as a list. Note: a `replacement` URL containing `:` (e.g. `http://$host:9022/...`) must be quoted inside a YAML flow `{...}` mapping.

## v0.4.5

- `proxy_ssl_verify` — whether to verify the upstream cert when proxying to an HTTPS backend. **Defaults off (matching nginx)**: proxying to a self-signed HTTPS backend (e.g. an internal two-tier proxy) now works out of the box instead of returning 502 on cert verification failure. Set `proxy_ssl_verify: on` to enforce verification (http/server/location, nearest wins).

## v0.4.4

Performance fix:

- **Backend idle connection pool default raised 16 → 100.** Load testing showed the old default starved mid/high concurrency: each request was forced to open/close a fresh backend TCP connection (connect/close syscall storm + TIME_WAIT buildup), halving throughput and pushing tail latency into the seconds. After the fix, the same load doubles throughput and drops p100 tail from ~2900ms to ~40ms. Very high concurrency should still set an explicit upstream `keepalive` matching the load (but not excessively large).
- Reverse proxy no longer probes for HTTP/2 (`ForceAttemptHTTP2: false`) — saves overhead when the backend is plain HTTP/1.1.
- New diagnostic: set `CREST_PPROF=127.0.0.1:6060` to enable Go pprof (off by default) for load-test profiling.

## v0.4.3

- `proxy_send_timeout` — timeout for writing the request to the backend (max gap between successful writes), inherited http < server < location. Completes the trio of nginx proxy timeouts.

## v0.4.2

**nginx parity (batch 2):**

- `limit_rate` / `limit_rate_after` — response bandwidth throttling; the first N
  bytes go full-speed, the rest are paced to the rate.
- `valid_referers` — anti-hotlinking: an invalid `Referer` gets 403; rules support
  `none` / `blocked` / `server_names` / domains (`*.foo.com`) / `~regex`.
- upstream `keepalive` — idle connection pool to backends (connection reuse, less
  handshake overhead under load).
- `gzip_static` — serve a precompressed `<file>.gz` when it exists and the client
  accepts gzip, skipping on-the-fly compression.

## v0.4.1

**nginx parity (batch 1):**

- `expires` — response caching headers (duration / `off` / `epoch` / `max` /
  negative no-cache), inherited http < server < location.
- `proxy_redirect` — rewrite the `Location` header from an upstream 3xx; `off`
  disables it, the default maps the `proxy_pass` URL back to the location prefix.
- `proxy_hide_header` / `proxy_pass_header` — control which upstream response
  headers pass through; `Date` / `Server` / `X-Accel-*` are hidden by default.
- `server_tokens` — whether the `Server` response header carries the version.
- `ssl_protocols` — configurable allowed TLS versions (http/server scope),
  default minimum TLS 1.2.

**Security:**

- **TLS 1.2 minimum by default** — TLS 1.0/1.1 are deprecated (RFC 8996) and now
  rejected; override with `ssl_protocols`.

**Admin console:**

- `ui.allow` — IP allow list (IP / CIDR). When bound to `0.0.0.0` only listed
  clients are admitted; loopback or a specific host IP already limits reach, so
  no list is needed.
- `-s reload` warns — on both the command line and the console — that `ui`
  changes need a full restart (the control channel is fixed at process start).
- Fix: the auto-opened admin URL uses `127.0.0.1` under a wildcard listen
  (`0.0.0.0` / `::`) so the browser can reach it.

## v0.4.0

## Highlights

- 🎛️ **Browser admin console** (`crest -s ui`) — live request stats with
  per-port filtering, in-browser config editor with syntax highlighting and
  validate-before-save, log viewer, one-click reload/stop.
- 📝 **Native YAML config** — alongside `nginx.conf` syntax, auto-detected by
  extension/content; unknown fields are rejected at validation time instead of
  being silently ignored.
- 📊 **Three-way runtime log split** — successful requests → `success.log`,
  failures (≥400) → `error.log`, process messages → `console.log`.
- 🔧 **One-command cross-platform build** (`build.sh`) — Windows (with icon) +
  Linux amd64/arm64 static binaries.
- 🐛 **Fix:** default IPv4-only listening (matching nginx) — resolves the
  dual-stack case where a `localhost` client arrives as `::1` and gets forwarded
  verbatim as `X-Real-IP`, crashing IPv4-only backends. Explicit IPv6 literals
  (`[::]:port`) still bind IPv6.

## Config enhancements

- `listen` accepts three forms: a bare port, nginx-style string shorthand
  (`"443 ssl default"`), or a mapping (`{port: 443, ssl: true}`; the older
  `{address: ...}` still works).
- Relative paths inside the config (root/alias/certs/htpasswd/logs/cache dirs)
  resolve against the **config file's directory**, not the working directory;
  missing roots/certs are warned at load time so `crest -t` catches them.
- New ops commands: `crest -s ui` (open console), `crest -s reopen` (log rotation).

## Full feature set

HTTP serving, reverse proxy, load balancing (weighted round-robin / least_conn /
ip_hash with max_fails / fail_timeout / backup / down health checks), rate
limiting (limit_req / limit_conn), proxy caching, TLS + SNI + automatic HTTP/2,
L4 proxying (stream, TCP/UDP), rewrite, try_files (incl. named locations
`@name`), gzip, error_page, add_header, allow/deny, auth_basic
(bcrypt/apr1/SHA1/plain), autoindex, map, proxy_set_header / proxy_cookie_path,
a variable system (`$uri` `$host` `$upstream_addr` `$proxy_add_x_forwarded_for`
…), and hot reload (`-s reload`, a bad config never takes down the running
server).

## Known limitations

- No automatic HTTPS (ACME/Let's Encrypt) — certificates are provided manually.
- No service discovery (Docker/Kubernetes).
- Reload has a millisecond port-handoff window (not zero-downtime).
- proxy_cache is cold after restart, ignores Vary; 8MB max per cached object.
- Log rotation on Windows is limited by file locking (not an issue on Linux).

## Downloads

| Platform | File |
|---|---|
| Windows x64 | `crest.exe` |
| Linux x64 | `crest-linux-amd64` |
| Linux arm64 | `crest-linux-arm64` |

Verify with `SHA256SUMS.txt`. Built with Go 1.20+; runs as a single static
binary with no runtime dependencies.
