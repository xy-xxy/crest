# gonx

A drop-in nginx alternative written in Go: single binary, zero runtime
dependencies, natively compatible with `nginx.conf`, plus a cleaner YAML config
format and a built-in browser admin console.

## v0.4.6

- `rewrites` — now accepts both a single-rule mapping `rewrites: {pattern: ..., replacement: ..., flag: redirect}` and a multi-rule list `rewrites: [{...}, {...}]`. Previously only a list was accepted, so a single rule still had to be written as a list. Note: a `replacement` URL containing `:` (e.g. `http://$host:9022/...`) must be quoted inside a YAML flow `{...}` mapping.

## v0.4.5

- `proxy_ssl_verify` — whether to verify the upstream cert when proxying to an HTTPS backend. **Defaults off (matching nginx)**: proxying to a self-signed HTTPS backend (e.g. an internal two-tier proxy) now works out of the box instead of returning 502 on cert verification failure. Set `proxy_ssl_verify: on` to enforce verification (http/server/location, nearest wins).

## v0.4.4

Performance fix:

- **Backend idle connection pool default raised 16 → 100.** Load testing showed the old default starved mid/high concurrency: each request was forced to open/close a fresh backend TCP connection (connect/close syscall storm + TIME_WAIT buildup), halving throughput and pushing tail latency into the seconds. After the fix, the same load doubles throughput and drops p100 tail from ~2900ms to ~40ms. Very high concurrency should still set an explicit upstream `keepalive` matching the load (but not excessively large).
- Reverse proxy no longer probes for HTTP/2 (`ForceAttemptHTTP2: false`) — saves overhead when the backend is plain HTTP/1.1.
- New diagnostic: set `GONX_PPROF=127.0.0.1:6060` to enable Go pprof (off by default) for load-test profiling.

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

- 🎛️ **Browser admin console** (`gonx -s ui`) — live request stats with
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
  missing roots/certs are warned at load time so `gonx -t` catches them.
- New ops commands: `gonx -s ui` (open console), `gonx -s reopen` (log rotation).

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
| Windows x64 | `gonx.exe` |
| Linux x64 | `gonx-linux-amd64` |
| Linux arm64 | `gonx-linux-arm64` |

Verify with `SHA256SUMS.txt`. Built with Go 1.20+; runs as a single static
binary with no runtime dependencies.
