# gonx

> 发布说明 / Release Notes：[中文](RELEASE_NOTES_CN.md) · [English](RELEASE_NOTES_EN.md) — 当前版本 v0.4.1

用 Go 写的类 nginx Web 服务器 / 反向代理。支持两种配置格式，按文件扩展名自动识别：

- **`.yml` / `.yaml`** —— gonx 原生 YAML 格式（推荐自用，见 `gonx.yml`）
- **其他（`.conf` 等）** —— nginx.conf 兼容语法（方便直接迁移已有配置）

两种格式解析后产出完全相同的内部结构，功能等价，随意选用。

## 快速开始

```sh
# YAML 格式启动
go run ./cmd/gonx -c gonx.yml

# nginx.conf 格式启动
go run ./cmd/gonx -c testdata/nginx_basic.conf

# 校验配置（等价于 nginx -t，两种格式都支持）
go run ./cmd/gonx -t -c gonx.yml

# 构建
go build -o gonx.exe ./cmd/gonx
```

## YAML 配置速览

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
    - listen: [8080, {address: 443, ssl: true, default: true}]
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

未知字段会直接报错（`KnownFields`），避免拼写错误被静默忽略。完整可运行示例见仓库根目录 `gonx.yml`（等价翻译自 `nginx.conf`）。

启动后访问 <http://localhost:8080/>（静态页）、<http://localhost:8080/ping>（return 指令）。

## 已支持的指令（Phase 1-4）

| 上下文 | 指令 |
|---|---|
| main | `http` `stream`（`events`/`worker_processes` 等会被接受但忽略） |
| http | `server` `upstream` `log_format` `access_log` `error_log` `root` `index` `gzip*` `limit_req_zone` `limit_conn_zone` `proxy_cache_path` `client_max_body_size` |
| server | `listen`（含 `default_server` / `ssl`） `server_name` `root` `index` `access_log` `location` `rewrite` `ssl_certificate` `ssl_certificate_key` `gzip*` `limit_req` `limit_conn` `proxy_cache` `proxy_cache_valid` `error_page` `client_max_body_size` |
| location | `root` `alias` `index` `proxy_pass` `return` `rewrite` `try_files`（含 `@named`） `gzip*` `limit_req` `limit_conn` `proxy_cache` `proxy_cache_valid` `proxy_set_header` `proxy_cookie_path` `client_max_body_size` `add_header` `allow`/`deny` `auth_basic`/`auth_basic_user_file` `autoindex` `proxy_connect_timeout`/`proxy_read_timeout` |
| upstream | `server`（含 `weight=` `max_fails=` `fail_timeout=` `backup` `down`） `least_conn` `ip_hash` |

`http`/`server` 也支持 `add_header` `allow`/`deny` `auth_basic*` `autoindex` `map` 及超时指令（就近覆盖）。
| stream | `upstream` `server` |
| stream server | `listen`（含 `udp`） `proxy_pass` `proxy_timeout` `proxy_connect_timeout` `proxy_responses` |

`gzip*` = `gzip` `gzip_types` `gzip_min_length` `gzip_comp_level`（http/server/location 三级继承）。

## 运行时日志

默认情况下（配置里没有显式写 `access_log` / `error_log`），gonx 把运行时日志按三类分流到**可执行文件同级的 `log/` 目录**：

| 文件 | 内容 |
|---|---|
| `log/success.log` | 成功的请求（HTTP 状态码 < 400） |
| `log/error.log` | 失败的请求（状态码 ≥ 400） |
| `log/console.log` | 进程消息（启动 / reload / 关闭）和运行错误，同时仍在终端显示 |

注意：这里的 `error.log` 记录**失败的 HTTP 请求**；进程级错误（对应 nginx 的 error_log）在 `console.log` 里。

- 目录基于**可执行文件位置**，不是当前工作目录——放哪跑就在哪建 `log/`。
- 配置里显式写了 `access_log` / `error_log` 时以配置为准，不再分流。
- `console.log` 里的启动 `listening on` 行是 info 级别，默认 error 级别会过滤掉；把 `error_log.level` 设成 `info` 即可看到。
- 配合 `gonx -s reopen` 可做日志轮转。

## 运维命令（对应 nginx -s）

```sh
gonx -s reload -c <conf>   # 热重载：新配置校验通过才会切换，失败保持旧配置继续跑
gonx -s reopen -c <conf>   # 重开日志文件（配合 logrotate 等外部轮转工具）
gonx -s status -c <conf>   # 查看运行状态
gonx -s stop   -c <conf>   # 优雅停止
gonx -s ui     -c <conf>   # 打开浏览器管理页面
```

## 管理页面

管理页面**跑在 gonx 服务进程内部**（控制通道就是它），有两种打开方式：

1. `gonx -s ui -c <conf>` —— 随时打开浏览器（适合默认的随机端口 + 随机 token）
2. 配置里固定地址，启动即可用（可收藏书签、可自动弹浏览器）：

```yaml
ui:
  listen: 127.0.0.1:8899   # 固定管理端口（默认随机）
  token: my-secret          # 固定访问令牌（默认每次启动随机生成）
  open: true                # 启动时自动打开浏览器
```

启动时管理页面地址会打印到终端和 console.log。`ui` 段修改后需重启生效（reload 不影响控制通道）。注意 `listen` 保持 127.0.0.1；绑定非回环地址会让局域网可访问管理台（gonx 会警告）。

页面功能：

- **请求统计**：总数 / 2xx / 3xx / 4xx / 5xx 实时刷新（自本次启动累计）
- **一键操作**：重载配置、重开日志、优雅停止
- **日志查看**：success.log / error.log / console.log 尾部 200 行，可自动刷新
- **配置在线编辑**：改完先经真实解析器校验（坏配置直接 400 拒绝），保存自动备份 `.bak`，可一键"保存并重载"

页面为单文件内嵌 HTML（无外部资源），自适应亮/暗色，Windows / Linux 通用。

## Linux 部署

纯 Go 无 cgo，交叉编译即可，产物是无依赖静态二进制（任意发行版 / scratch 容器可用）：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o gonx ./cmd/gonx
# arm64: GOARCH=arm64
```

Linux 上额外注意：监听 80/443 需要 root 或 `setcap 'cap_net_bind_service=+ep' gonx`；日志轮转可用标准 logrotate（`mv` + `gonx -s reopen`），没有 Windows 的文件锁限制。

实现方式：主进程在 127.0.0.1 上开一个带 token 的控制端口，信息写入 `<conf>.ctl`（0600 权限），`-s` 命令读它发请求——Windows 没有 SIGHUP，这个方案跨平台一致。注意 reload 是"先建新实例、关旧监听、再起新监听"，端口交接有毫秒级窗口，不是 nginx 那种零停机 fd 传递。

行为对齐 nginx 的关键语义：

- **location 匹配**：`=` 精确 → `^~` 前缀短路 → `~`/`~*` 正则按配置顺序 → 最长普通前缀
- **server_name 匹配**：精确 → `*.example.com`（最长）→ `www.*`（最长）→ `~正则` → default server
- **root vs alias**：`root` 拼接完整 URI，`alias` 替换 location 前缀
- **proxy_pass**：带 URI 时替换 location 前缀（`/api/x` + `http://b/v1/` → `/v1/x`）；不带则透传原始 URI；默认 `Host: $proxy_host`，并注入 `X-Real-IP` / `X-Forwarded-For` / `X-Forwarded-Proto`
- **upstream**：平滑加权轮询（与 nginx 同算法）；连接失败自动切换下一台并将故障机摘除 10s（对应默认 `fail_timeout`）
- **access_log**：内置 `combined` 格式；`log_format` 自定义（支持 `$remote_addr` `$request` `$status` `$body_bytes_sent` `$http_referer` `$http_user_agent` `$host` `$request_time` 等）；路径可写 `stdout` / `stderr` / 文件
- **变量**：`return`/`rewrite`/`try_files` 中支持 `$uri` `$request_uri` `$args` `$is_args` `$host` `$scheme` `$remote_addr` `$request_method` `$http_*` `$arg_*` `$cookie_*` 等，`${name}` 写法亦可
- **rewrite**：`last`/`break`/`redirect`/`permanent` 四个 flag，`$1`-`$9` 捕获组，替换串带 `?` 时的参数合并/尾部 `?` 丢弃参数与 nginx 一致；`last` 会重新进行 location 匹配（循环上限 10 次）
- **try_files**：`try_files $uri $uri/ /fallback;`，最后一项为内部跳转或 `=404` 式状态码，SPA 场景可直接用
- **HTTPS**：`listen 443 ssl` + `ssl_certificate`/`ssl_certificate_key`，SNI 按 `server_name` 选证书；Go 标准库自动协商 **HTTP/2**
- **gzip**：按 Content-Type 过滤（`text/html` 恒压缩），`gzip_min_length` 阈值，206/已编码响应自动跳过
- **负载均衡**：默认平滑加权轮询；`least_conn`（按 active/weight 比例）；`ip_hash`（客户端粘住同一后端，故障顺延）
- **limit_req**：nginx 同款漏桶算法（`rate=Nr/s|r/m`、`burst=`、`nodelay`），超限 503，未超限但排队的请求按速率延迟放行
- **limit_conn**：按 key 的并发计数，超限 503
- **proxy_cache**：磁盘存储 + 内存索引；TTL 优先级为上游 `Cache-Control`/`Expires` > `proxy_cache_valid`；`no-store`/`private`/`Set-Cookie`/带 `Authorization` 的请求不缓存；响应带 `X-Cache-Status: HIT|MISS`。重启后缓存冷启动（索引不持久化），暂不处理 `Vary`
- 未知指令会打印警告并忽略，不会导致启动失败——真实的 nginx.conf 大多可以直接喂进来

- **stream（L4）**：TCP 与 UDP 代理，支持 upstream 负载均衡与故障切换；UDP 按客户端地址维护会话，`proxy_responses` 控制会话提前回收
- **error_page**：拦截 gonx 自身产生的错误（404/502/503 等）内部跳转到自定义页面并保留原状态码；上游返回的错误默认透传（对齐 `proxy_intercept_errors off`，API 的 JSON 错误不会被换成 HTML）
- **client_max_body_size**：超限返回 413，chunked 请求边读边限
- **add_header**：支持 `always` 参数；遵循 nginx 就近覆盖（location 有则屏蔽 server/http 层）
- **allow / deny**：IP / CIDR / `all` 访问控制，按顺序匹配，命中即决定
- **auth_basic**：HTTP 基础认证，htpasswd 支持 bcrypt / Apache-MD5(`$apr1$`) / SHA1 / 明文
- **autoindex**：目录列表（nginx 风格 HTML）
- **命名 location `@name`**：配合 `try_files $uri @backend` 做"静态优先、回落代理"
- **map 指令 + `$upstream_addr`**：变量系统扩展到映射变量与上游地址，可用于 `log_format`
- **upstream 健康**：`max_fails` / `fail_timeout` 滑动窗口失败计数、`backup` 备用节点、`down` 手动摘除，成功即重置失败计数
- **proxy 超时**：`proxy_connect_timeout` / `proxy_read_timeout` 就近覆盖
- **日志轮转**：`gonx -s reopen` 重开文件句柄，配合外部 logrotate

## 限制与 Roadmap

单进程多 goroutine 模型，无 master/worker。同一端口不能混用 ssl 和非 ssl。带 body 的请求在 upstream 失败时不重试（body 可能已被消费）。单个缓存对象上限 8MB（超出只透传不缓存）。

- **待办**：FastCGI（需要 PHP 场景时再做）、`proxy_cache` 的 Vary 支持与索引持久化、stream 的 TLS 终结

## 构建

```sh
go build -o gonx.exe ./cmd/gonx    # cmd/gonx/rsrc_windows.syso 会自动嵌入图标
go run ./tools/genicon -o cmd/gonx/gonx.ico   # 重新生成图标（如需改设计）
```

## 代码结构

```
cmd/gonx/            入口（-c/-t/-v，优雅关闭）
internal/config/     nginx.conf 词法/语法分析 → AST → 强类型配置
internal/server/     listen 地址聚合、vhost 匹配、location 匹配、访问日志
internal/handler/    静态文件（root/alias/index）、反向代理（含 upstream 重试）
internal/upstream/   平滑加权轮询池
internal/logging/    access log（可自定义格式）、分级 error log
```

## 测试

```sh
go test ./...
```

覆盖：配置解析（含引号/注释/错误恢复）、location/server_name 匹配矩阵、WRR 权重分布与摘除、端到端（静态 + 反代 + URI 替换 + upstream 故障切换 + 多虚拟主机）。
