# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

gonx is a single-binary nginx-compatible web server / reverse proxy in Go (no cgo, stdlib + `x/crypto` + `yaml.v3` only). It accepts both `nginx.conf` syntax and a native YAML format, and hosts a browser admin console inside the server process. `README.md` is the user-facing reference for supported directives and nginx-aligned semantics — consult it before adding or changing directive behavior.

## Commands

```sh
go build -o gonx.exe ./cmd/gonx      # cmd/gonx/rsrc_windows.syso embeds the Windows icon
./build.sh                           # all release targets: Windows + Linux amd64/arm64

go run ./cmd/gonx -c gonx.yml                    # run with YAML config
go run ./cmd/gonx -c testdata/nginx_basic.conf   # run with nginx.conf config
go run ./cmd/gonx -t -c gonx.yml                 # validate config only (like nginx -t)

go test ./...
go test ./internal/server -run TestTryFiles -v   # single test
go test ./internal/config -v                     # one package

go run ./tools/genicon -o cmd/gonx/gonx.ico -png gonx.png -png-size 200
```

`GONX_PPROF=127.0.0.1:6060` enables net/http/pprof on a running instance.

## Architecture

### Config: two front-ends, one typed struct

`config.LoadFile` picks the format by extension, falling back to content sniffing (`looksLikeYAML`), then anchors every relative filesystem path to the **config file's directory** (`resolvePaths`) so behavior is independent of the working directory. It also appends `Warnings` for missing roots/certs, which `-t` prints.

- nginx path: `lexer.go` → `parser.go` (generic `Directive` AST in `ast.go`) → `loader.go` (`Load`, ~1600 lines of per-directive handling).
- YAML path: `yaml.go` has a parallel handwritten schema (`yamlRoot` … `toConfig`) decoded with `KnownFields(true)` so typos are errors.

**Both must produce the identical `*config.Config`.** Adding a directive means touching the typed struct in `loader.go`, the nginx loader, the YAML schema + `toConfig`, the server wiring, and usually the config-help drawer in `cmd/gonx/ui.go`. Tests should cover both front-ends (`internal/server/e2e_test.go` builds from nginx syntax, `yaml_e2e_test.go` from YAML).

### Server: config → handler tree, built once at startup

`server.NewWithOptions` compiles the whole config into an immutable `*Instance`: one `AddrServer` per listen address → `hostGroup` of `vhost`s (server_name matching in `vhost.go`) → `locationSet` of `route`s (nginx match precedence in `location.go`). Nothing is re-resolved per request; scope inheritance (http → server → location) is flattened at build time by the `boolOr` / `durOr` / `resolve*` / `pick*` helpers in `server.go` — "nearest scope wins" is implemented there, not at request time.

Per-request flow (`AddrServer.ServeHTTP` → `dispatch`): attach map/`$upstream_addr` resolvers → vhost match → server rewrites → bounded 10-cycle loop of *location match → location rewrites → try_files → handler* (a `last` rewrite or a try_files fallback re-enters the loop) → status counters → access log.

Middleware is wrapped in `buildRoute`, and **wrap order is the request order reversed**. Innermost is the terminal handler from `buildHandler` (`returnHandler` | `handler.Proxy` | `handler.Static`), then cache, gzip, add_header, expires, limit_rate, limit_req/limit_conn, body-size limit, valid_referers, and outermost access/auth. Inserting a new policy in the wrong position changes observable semantics (e.g. gzip must sit outside the cache so cached bytes stay uncompressed).

Response interception is done with layered `http.ResponseWriter` wrappers in `handler.go` / `policy.go` (`respWriter` counts bytes/status, `errorPageWriter` intercepts gonx-generated errors but not upstream ones, `statusOverrideWriter`, `headerAdder`, `expiresWriter`). Each wrapper must forward `Flush` and `Hijack` or streaming responses and WebSocket upgrades break.

### Control channel and reload

There is no master/worker and no SIGHUP. The process opens a token-guarded loopback HTTP server (`cmd/gonx/main.go: startControl`) and writes `<addr> <token>` to `<conf>.ctl` (mode 0600); `gonx -s reload|reopen|stop|status|ui` is an HTTP client reading that file. The admin UI (`cmd/gonx/ui.go`, one self-contained HTML string in `uiHTML`) is served on the same channel.

`runner.reload()` builds a *complete new* `Instance` before shutting the old one down, so an invalid config never takes down the running server — but old listeners must close before new ones bind, so there is a millisecond port gap. The `ui` section is fixed at process start; reload detects a change and returns a warning note instead of applying it.

### Other packages

`internal/handler` (static with root/alias/index/autoindex, reverse proxy with retry+failover, gzip, cache middleware, rate limiting) · `internal/upstream` (`Pool` interface; smooth weighted round-robin, least_conn, ip_hash, plus `max_fails`/`fail_timeout`/`backup`/`down` health state on `Backend`) · `internal/vars` (nginx `$variables`, with a per-request context resolver for `map` targets and `$upstream_addr`) · `internal/logging` (`AccessLogger` interface — a single log or the success/fail split both satisfy it; leveled error log) · `internal/limiter`, `internal/cache`, `internal/auth`, `internal/stream` (L4 TCP/UDP).

Default runtime logs (`Options.LogDir`, when the config sets no explicit paths) go to `log/` **next to the executable**, split into `success.log` / `error.log` / `console.log`.

## Conventions

- Code comments, warning text, and the admin UI mix English and Chinese; match the surrounding file.
- Unknown nginx directives are warned-and-ignored (`Config.warnf`), never fatal — real nginx configs should mostly run as-is.
- Bumping the version means updating `cmd/gonx/main.go` (`const version`), the README badge and version line, and both `RELEASE_NOTES_CN.md` / `RELEASE_NOTES_EN.md`.
- Server tests are grouped as `phase2_test.go` … `phase5_test.go` by the feature batch they were added in; use `buildInstance` (nginx syntax) / `buildFromYAML` + `httptest` for new end-to-end cases.
- Built binaries (`gonx.exe`, `gonx-linux-*`) are committed at the repo root; `log/`, `dist/` and `*.ctl` are gitignored.
