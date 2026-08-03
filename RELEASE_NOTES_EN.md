# gonx v0.4.0

A drop-in nginx alternative written in Go: single binary, zero runtime
dependencies, natively compatible with `nginx.conf`, plus a cleaner YAML config
format and a built-in browser admin console.

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
