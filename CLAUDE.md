# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

crest is a single-binary nginx-compatible web server / reverse proxy in Go (no cgo, stdlib + `x/crypto` + `x/sys` (Windows SCM) + `yaml.v3`). It accepts both `nginx.conf` syntax and a native YAML format, and hosts a browser admin console inside the server process. `README.md` is the user-facing reference for supported directives and nginx-aligned semantics — consult it before adding or changing directive behavior.

## Commands

```sh
go build -o dist/crest ./cmd/crest         # native host build
./build.sh                           # dist/: Windows amd64 + Win7 amd64/386 + Linux amd64/arm64

./crest                                          # no args: auto-detects crest.yml/.yaml/.conf (double-click case)
go run ./cmd/crest -c crest.yml                    # run with YAML config
go run ./cmd/crest -c testdata/nginx_basic.conf   # run with nginx.conf config
go run ./cmd/crest -t -c crest.yml                 # validate config only (like nginx -t)

go test ./...
go test -race ./...                              # proxy_buffering path is concurrent; run before releases
go test ./internal/server -run TestTryFiles -v   # single test
go test ./internal/config -v                     # one package

GOOS=windows go vet ./cmd/crest                  # type-check build-tagged Windows service code from macOS/Linux
W=~/.local/share/crest-build/go1.20.14/go        # Win7 SDK (same default as build.sh / WIN7_GOROOT)
GOROOT=$W GOENV=off GOTOOLCHAIN=local GOOS=windows GOARCH=386 $W/bin/go vet ./...   # Go 1.20 API check

go run ./tools/genicon -o cmd/crest/crest.ico -syso cmd/crest/rsrc_windows_amd64.syso \
    -png crest.png -png-size 200
go run ./tools/genicon -o cmd/crest/crest.ico -arch 386 \
    -syso cmd/crest/rsrc_windows_386.syso
```

`CREST_PPROF=127.0.0.1:6060` enables net/http/pprof on a running instance.

CI (`.github/workflows/ci.yml`, GitHub remote only; `origin` is Gitee) runs gofmt, vet and tests on Linux/macOS/Windows (`-race` except Windows), a Go 1.20.14 vet+test job, and `./build.sh` with `WIN7_GOROOT` pointed at that toolchain.

## Architecture

### Config: two front-ends, one typed struct

`config.LoadFile` picks the format by extension, falling back to content sniffing (`looksLikeYAML`), then anchors every relative filesystem path to the **config file's directory** (`resolvePaths`) so behavior is independent of the working directory. It also appends `Warnings` for missing roots/certs, which `-t` prints.

- nginx path: `lexer.go` → `parser.go` (generic `Directive` AST in `ast.go`) → `loader.go` (`Load`, ~1600 lines of per-directive handling). The lexer resolves nginx's escapes (`\n` `\t` `\r` `\"` `\'` `\\`) in every argument. The exception is directives listed in `pathDirectives` (`parser.go`), which keep backslashes as written so Windows paths survive. A new directive that takes a filesystem path must be added there.
- YAML path: `yaml.go` has a parallel handwritten schema (`yamlRoot` … `toConfig`) decoded with `KnownFields(true)` so typos are errors.

**Both must produce the identical `*config.Config`.** Adding a directive means touching the typed struct in `loader.go`, the nginx loader, the YAML schema + `toConfig`, the server wiring, and usually the config-help drawer in `cmd/crest/ui.go`. Tests should cover both front-ends (`internal/server/e2e_test.go` builds from nginx syntax, `yaml_e2e_test.go` from YAML).

### Server: config → handler tree, built once at startup

`server.NewWithOptions` compiles the whole config into an immutable `*Instance`: one `AddrServer` per listen address → `hostGroup` of `vhost`s (server_name matching in `vhost.go`) → `locationSet` of `route`s (nginx match precedence in `location.go`). Nothing is re-resolved per request; scope inheritance (http → server → location) is flattened at build time by the `boolOr` / `durOr` / `resolve*` / `pick*` helpers in `server.go` — "nearest scope wins" is implemented there, not at request time.

Per-request flow (`AddrServer.ServeHTTP` → `dispatch`): attach map/`$upstream_addr` resolvers → vhost match → real_ip → server rewrites → bounded 10-cycle loop of *location match → location rewrites → try_files → handler* (a `last` rewrite or a try_files fallback re-enters the loop) → status counters → access log.

real_ip (`realip.go`) overwrites `r.RemoteAddr` in place right after the vhost match, at most once per request, so everything that needs the client address must read `r.RemoteAddr` (always `ip:port`, port 0 if unknown) after that point. A location's own `set_real_ip_from` is the outermost wrapper in `buildRoute`. The TCP peer is only available as `$realip_remote_addr`; the admin channel's `ui.allow` uses its own connection address and is unaffected.

`block_actuator` (`security.go`, on by default, http/server scope) returns 403 for any `/actuator` path segment *before* location handlers, so no location or cache can override it. It is re-checked after every step that can change `r.URL.Path` (location rewrites, try_files, `error_page` sub-requests) and bypasses `errorPageWriter`. A new internal-redirect path in `dispatch` needs the same check.

Middleware is wrapped in `buildRoute`, and **wrap order is the request order reversed**. Innermost is the terminal handler from `buildHandler` (`returnHandler` | `handler.Proxy` | `handler.Static`), then cache, gzip, add_header, expires, limit_rate, limit_req/limit_conn, body-size limit, valid_referers, access/auth, and outermost a location-level real_ip. Inserting a new policy in the wrong position changes observable semantics (e.g. gzip must sit outside the cache so cached bytes stay uncompressed).

Response interception is done with layered `http.ResponseWriter` wrappers in `handler.go` / `policy.go` (`respWriter` counts bytes/status, `errorPageWriter` intercepts crest-generated errors but not upstream ones, `statusOverrideWriter`, `headerAdder`, `expiresWriter`). Each wrapper must forward `Flush` and `Hijack` or streaming responses and WebSocket upgrades break.

### Control channel and reload

There is no master/worker and no SIGHUP. The process opens a token-guarded loopback HTTP server (`cmd/crest/main.go: startControl`) and writes `<addr> <token>` to `<conf>.ctl` (mode 0600); `crest -s reload|reopen|stop|status|ui` is an HTTP client reading that file. The admin UI (`cmd/crest/ui.go`, one self-contained HTML string in `uiHTML`) is served on the same channel.

`runner.reload()` builds a *complete new* `Instance` and starts it with `StartReplacing(old)` (`internal/server/handoff.go`). Addresses only the new config uses are bound first; if any bind fails, the new instance's sockets are closed and the old instance keeps serving. Sockets on addresses both configs use (keyed by family + canonical address via `addrKey`) move to the new instance while they stay bound. Each server accepts through a closable `listenerView` over the shared `queuedListener`, so the old instance can stop accepting without closing the socket and no connection is refused. The old instance then drains in the background for up to `drainTimeout` and closes only the sockets nobody took over. A UDP stream is handed over by waking the old read loop with a past read deadline, not by closing the socket. The new instance is built with `NewReplacing(cfg, opts, old)` under `ru.mu`. Zones whose definition is unchanged (same name + key for limit_req/limit_conn, same dir for proxy_cache) reuse the old instance's objects; see the `kept*` helpers in `handoff.go`. Each `limit_req` wrapper passes its own config's rate to `TakeAt`. Reused caches stay in `borrowedCaches` until `StartReplacing` commits, and `Shutdown` closes only owned caches, so a rejected reload cannot delete the running cache. New per-instance state that should survive a reload needs the same borrow-then-commit treatment. Once an instance has stopped accepting, its serve errors are suppressed (`i.closed`); otherwise its `ErrClosed` would reach `ru.errs` and exit the process. The `ui` section is fixed at process start; reload detects a change and returns a warning note instead of applying it.

### Service mode

`crest install|uninstall|start|stop|restart|status` are positional subcommands that `dispatchServiceCommand` (`cmd/crest/service.go`) handles before normal flag parsing. They are separate from `-s ...`: `status` asks the OS service manager, while `-s status` asks the control API. The `serviceManager` interface is implemented per OS with build tags: `service_linux.go` writes a systemd unit and sends `sd_notify` readiness, `service_windows.go` uses the SCM through `x/sys/windows/svc`, and `service_other.go` returns an unsupported error (this includes macOS). The registered command is the internal `run-service -c <absolute path>`, with the config's directory as the working directory. Install refuses to overwrite an existing registration. On both platforms `runAsService` calls the same `runServer` as foreground mode and passes a `serviceLifecycle` (stop channel plus a started callback for readiness). A non-nil lifecycle suppresses `ui.open`.

### Other packages

`internal/handler` (static with root/alias/index/autoindex, reverse proxy with retry+failover, gzip, cache middleware, rate limiting) · `internal/upstream` (`Pool` interface; smooth weighted round-robin, least_conn, ip_hash, plus `max_fails`/`fail_timeout`/`backup`/`down` health state on `Backend`) · `internal/vars` (nginx `$variables`, with a per-request context resolver for `map` targets and `$upstream_addr`) · `internal/logging` (`AccessLogger` interface — a single log or the success/fail split both satisfy it; leveled error log) · `internal/limiter`, `internal/cache`, `internal/auth`, `internal/stream` (L4 TCP/UDP).

Default runtime logs (`Options.LogDir`, when the config sets no explicit paths) go to `log/` **next to the executable**, split into `success.log` / `error.log` / `console.log`.

## Conventions

- Code comments, warning text, and the admin UI mix English and Chinese; match the surrounding file.
- Unknown nginx directives are warned-and-ignored (`Config.warnf`), never fatal — real nginx configs should mostly run as-is.
- Bumping the version means updating `cmd/crest/main.go` (`const version`), the README badge and version line, and both `RELEASE_NOTES_CN.md` / `RELEASE_NOTES_EN.md`.
- Server tests are grouped as `phase2_test.go` … `phase5_test.go` by the feature batch they were added in; use `buildInstance` (nginx syntax) / `buildFromYAML` + `httptest` for new end-to-end cases.
- Build binaries and release archives only under `dist/` (or a versioned subdirectory). Do not add root-level executables or checksum files to Git. `build.sh` defaults to `dist/` and accepts `OUTPUT_DIR`; keep `log/`, `dist/` and control files ignored.
- Every package build must include both `crest-win7-amd64.exe` and `crest-win7-386.exe`, built with the independent Go 1.20.14 SDK. Keep both architecture-specific Windows icon resources.
- Because of the Win7 targets, all code must compile with **Go 1.20**. `go 1.20` in `go.mod` blocks newer language features (`min`/`max`, range-over-int) under the local Go 1.26 toolchain, but it does **not** block newer stdlib packages or APIs such as `slices`, `maps`, `cmp`, `log/slog` or `sync.OnceFunc`. Those compile locally and only fail in `build.sh`, so run the Go 1.20 vet command above. Dependency bumps must also keep supporting Go 1.20.
- `docs/performance.md` (Chinese) records the validated v1.0.0 concurrency/memory matrix and known platform limits; keep any performance claims in the README or release notes consistent with it.
