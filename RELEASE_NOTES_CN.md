# 更新日志

本项目遵循语义化版本（0.x 阶段，次版本号递增表示新增功能）。

## v0.4.3

- `proxy_send_timeout` —— 写后端请求的超时（两次写操作之间的最大间隔），http/server/location 就近覆盖。补齐 nginx 三个 proxy 超时的最后一个。

## v0.4.2

对齐 nginx 的功能补齐（第二批）：

- `limit_rate` / `limit_rate_after` —— 响应带宽限速；前 N 字节全速、之后按速率节流。
- `valid_referers` —— 防盗链：非法 `Referer` 返回 403；规则支持 `none` / `blocked` / `server_names` / 域名（`*.foo.com`）/ `~正则`。
- upstream `keepalive` —— 到后端的空闲长连接池（连接复用，降低高并发下的握手开销）。
- `gzip_static` —— 存在 `<文件>.gz` 且客户端支持 gzip 时，直接发送预压缩文件，省去实时压缩。

## v0.4.1

对齐 nginx 的功能补齐（第一批）：

- `expires` —— 响应缓存头（时长 / `off` / `epoch` / `max` / 负值 no-cache），http/server/location 就近覆盖。
- `proxy_redirect` —— 改写后端 3xx 的 `Location` 头；`off` 关闭，缺省按 `proxy_pass` → location 前缀自动改写。
- `proxy_hide_header` / `proxy_pass_header` —— 控制后端响应头透传；默认隐藏 `Date` / `Server` / `X-Accel-*`。
- `server_tokens` —— 控制 `Server` 响应头是否带版本号（安全加固）。
- `ssl_protocols` —— 可配置允许的 TLS 版本（http/server 层），缺省最低 TLS 1.2。

安全：

- **默认最低 TLS 1.2** —— TLS 1.0/1.1 已被 RFC 8996 弃用，现默认拒绝；可用 `ssl_protocols` 覆盖。

管理页面：

- `ui.allow` —— 管理页面 IP 白名单（IP / CIDR）。绑 `0.0.0.0` 时只放行名单内的客户端；回环或具体本机 IP 已限制访问面，无需配置。
- `-s reload` 检测到 `ui` 段变更时，在命令行和管理页面均提示"需完全重启才生效"（控制通道在进程启动时固定）。
- 修复：管理页面自动打开的地址在通配监听（`0.0.0.0` / `::`）下换成 `127.0.0.1`，浏览器可直接访问。

## v0.4.0

首个正式发布版本。gonx 是一个用 Go 编写的 nginx 替代品：单文件、无外部依赖，
配置文件直接兼容 nginx.conf，同时提供更清爽的原生 YAML 格式和内置的浏览器管理控制台。

### 亮点

- **原生 YAML 配置** —— 除 nginx.conf 语法外新增 YAML 格式，按扩展名 / 内容自动识别；
  未知字段在校验时直接报错，避免拼写错误被静默忽略。
- **浏览器管理控制台** —— `gonx -s ui` 或配置 `ui` 段一键打开：
  - 请求统计（总数 / 2xx / 3xx / 4xx / 5xx，支持按监听端口筛选）
  - 一键重载配置 / 重开日志 / 优雅停止
  - 日志查看（success / error / console，带行号、自动刷新、粘底滚动）
  - 在线配置编辑器（VS Code 风格语法高亮 + 行号，保存前经真实解析器校验并自动备份）
  - 右侧配置帮助抽屉：完整配置示例（含全部指令，带注释）+ 分组速查 + 关键字搜索
- **三路运行时日志分流** —— 未显式配置 access_log 时，自动按可执行文件同级 `log/` 目录分流：
  成功请求 → `success.log`，失败请求（≥400）→ `error.log`，进程消息 → `console.log`。
- **跨平台一键构建** —— `build.sh` 同时产出 Windows(带图标) + Linux amd64/arm64 静态二进制。

### 配置增强

- `listen` 支持三种写法：端口、nginx 风格字符串简写（`"443 ssl default"`）、
  映射（`{port: 443, ssl: true}`，旧的 `{address: ...}` 仍兼容）。
- 配置内的相对路径（root/alias/证书/htpasswd/日志/缓存目录）按**配置文件所在目录**解析，
  不再受启动工作目录影响；加载时对不存在的 root/证书等目录发出警告，`-t` 即可发现。
- 运维命令新增 `gonx -s ui`（打开控制台）、`gonx -s reopen`（配合日志轮转）。

### 修复

- **默认 IPv4-only 监听**（对齐 nginx）：修复 dual-stack 下 `localhost` 客户端以 `::1`
  到达、并被原样通过 `X-Real-IP` 转发给后端、导致仅识别 IPv4 的后端崩溃的问题。
  显式 IPv6 字面量（`[::]:port`）仍监听 IPv6。
- 配置高亮 / 页面布局若干细节修复。

### 完整功能

HTTP 服务、反向代理、负载均衡（加权轮询 / least_conn / ip_hash，含 max_fails / fail_timeout /
backup / down 健康检查）、限流（limit_req / limit_conn）、代理缓存（proxy_cache）、
TLS + SNI + 自动 HTTP/2、L4 代理（stream，TCP / UDP）、rewrite、try_files（含命名 location
`@name`）、gzip、error_page、add_header、allow/deny、auth_basic（bcrypt/apr1/SHA1/明文）、
autoindex、map、proxy_set_header / proxy_cookie_path、变量系统（`$uri` `$host` `$upstream_addr`
`$proxy_add_x_forwarded_for` 等）、热重载（`-s reload`，坏配置不影响运行中的服务）。

### 已知限制

- 无自动 HTTPS（ACME/Let's Encrypt），需手动提供证书。
- 无服务发现（Docker/Kubernetes）。
- reload 端口交接存在毫秒级窗口，非零停机。
- proxy_cache 重启后冷启动、不处理 Vary；单缓存对象上限 8MB。
- Windows 上日志轮转受文件锁限制（Linux 无此问题）。

### 环境

- 构建需 Go 1.20+；运行为单文件静态二进制，无运行时依赖。
- 依赖：`golang.org/x/crypto`（bcrypt）、`gopkg.in/yaml.v3`，均为纯 Go。
- 测试：11 个包，63 个测试函数，全部通过。
