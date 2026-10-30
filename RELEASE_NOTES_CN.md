# 更新日志

本项目遵循语义化版本。

## v1.1.0 — 2026-10-30

- 新增 `install / start / stop / restart / status / uninstall` 服务管理命令。
  Windows 使用原生 SCM（含独立 Win7 构建），Linux 使用 systemd。
- `install` 验证配置后注册并设置开机自启；`install --start` 注册后立即启动，
  `install --dry-run` 只显示计划。Windows 需管理员终端，Linux 注册需 root。
- 固定 exe、配置和工作目录的绝对路径，支持空格/中文路径；拒绝覆盖已有服务。
  卸载会停止并注销服务，保留程序、配置和业务文件。
- 服务启动/停止接入原有服务器生命周期，启动失败会报告给服务管理器；systemd
  在实际启动后接收 READY 通知。服务模式不自动打开浏览器，日志优先写入文件。
- 构建默认输出至 `dist/`，移除仓库根目录的旧二进制和校验文件；从 GitHub Release
  获取发行包，源码仓库保留配置、证书示例和图标资源。

验证：全量及 race 测试；Linux 隔离 VM 的真实安装、启动、重载、重启、停止、
卸载流程与特殊路径；Windows/Win7 的程序和测试代码交叉编译。尚未在 Windows
真机注册验证。v1.0.0 记录的性能边界与已知限制继续适用。

## v1.0.0 — 2026-10-30

- 修复突发反代请求中的建连压力：后端每主机默认空闲池从 100 增至 512；
  每个代理 Transport 最多同时发起 16 个 TCP 建连，排队遵守请求取消及
  `proxy_connect_timeout`。不限制已建立的流式连接，显式 `keepalive` 仍优先。
- 每个 HTTP 监听增加固定容量 512 的 TCP 接收队列，将接收连接与 HTTP/TLS
  初始化分离；关闭时清理未分发连接，活跃请求仍走正常优雅关闭流程。
- 限制整个进程最多同时录制 4 个缓存对象，每个响应体上限 8 MiB；预算用尽时
  直接流式透传并返回 `X-Cache-Status: BYPASS`。不可缓存响应、SSE、HEAD
  未命中不再积攒完整响应体，HEAD 未命中正确保留状态码和响应头。
- 缓存文件采用不可变的独立版本，避免并发替换时旧响应头搭配新响应体；最后
  一个读者关闭后才清理旧文件，正常停机清理本实例的缓存文件。完善 Vary 多值
  请求头与策略顺序规范化。
- 修复代理缓冲器等待下一段响应数据时的空转；继续保证可缓存响应在发送给
  客户端前完成缓存发布，避免紧随请求抢在写入前未命中。
- 增加可复现并发/RSS 压测工具、回归测试和[完整报告](docs/performance.md)，
  保留修复前的失败数据。
- 构建脚本增加独立 Go 1.20.14 的 Win7 目标和 `OUTPUT_DIR`，可在独立目录
  打包，保留现有可执行文件和 SDK。

已知限制：macOS 冷启动回环突发仍偶有连接重置；Linux 额外重复测试的 P99
延迟曾达 4.2 秒，不作低延迟 SLO 保证。压测结果只覆盖报告中的负载和环境，
不保证无限并发或固定 RSS；`GOMEMLIMIT`
为软限制，部署时仍需配置并发上限并预留系统资源。压测平台为 macOS ARM64 与隔离 Linux ARM64 虚拟机，
Windows/Win7/Linux 产物为交叉编译，未进行对应真机压测；Win7 使用旧版工具链。
缓存索引持久化、FastCGI、stream TLS 终止仍为后续工作。

## v0.7.0

新增与修复：

- 新增令牌保护的 JSON `/healthz`、`/readyz`；只有 HTTP 或 stream listener
  成功绑定后才就绪，Shutdown 开始时立即变为未就绪。
- `/status` 保留兼容的一行文本格式，并支持 `Accept: application/json` 或
  `/status?format=json` 返回结构化状态。
- `proxy_cache` 正式支持 Vary 变体和常数时间索引；`Vary: *` 不缓存，GET
  写入共享条目，HEAD 可读取但不会用空 body 污染 GET。
- 反代处理结束后恢复原始请求的 Host 和 URL，避免复用 request 对象时改变
  缓存键；可缓存响应在释放给客户端前先完成写盘和索引更新，避免紧随其后的
  请求与缓存写入竞态。

缓存索引仍只在内存中，重启后冷启动；FastCGI 与 stream TLS 终止仍属于后续工作。

## v0.6.0

**项目更名：gonx → crest。** 原名带着 nginx 的影子，而这个项目早已不只是「Go 写的 nginx」。crest 取「堰顶」之意 —— 水流越过堰坝的那道脊线，正是流量被引导、分流、限速的位置。

升级需要手动调整（以下标识全部改变）：

| 项 | 旧 | 新 |
|---|---|---|
| 可执行文件 | `gonx.exe` / `gonx-linux-*` | `crest.exe` / `crest-linux-*` |
| 默认配置文件 | `gonx.conf` | `crest.conf` |
| 控制文件 | `<conf>.ctl` | 不变 |
| 管理令牌请求头 | `X-Gonx-Token` | `X-Crest-Token` |
| 管理台 Cookie | `gonx_token` | `crest_token` |
| pprof 环境变量 | `GONX_PPROF` | `CREST_PPROF` |
| `Server` 响应头 | `gonx/<版本>` | `crest/<版本>` |
| Go 模块路径 | `gonx` | `crest` |

配置文件内容本身**无需改动** —— 指令语法、YAML 字段名一律不变，把 `gonx.yml` 改名为 `crest.yml`（或沿用任意文件名，用 `-c` 指定）即可。运维命令从 `gonx -s reload` 变成 `crest -s reload`。

**省略 `-c` 时自动探测配置文件。** 此前默认值固定为 `crest.conf`，而推荐格式是 YAML，于是在 Windows 上双击 exe 往往直接失败、控制台一闪而过。现在省略 `-c` 时依次查找工作目录、再查可执行文件所在目录下的 `crest.yml` / `crest.yaml` / `crest.conf`，`-s reload` 等运维命令同样适用。找不到时会列出全部找过的路径而不只说一句"找不到配置"，并且这条错误会写进 `log/console.log` —— 双击窗口关掉之后仍可追查。显式 `-c` 的行为不变。

图标一并重做：旧图标是 nginx 绿配白色字母 G，两个元素都属于旧身份。新图标为深蓝到青的渐变底 + 两道白色水线（上为浪峰、下为余波），呼应 crest 本义。`genicon` 现在还能直接生成 Windows 资源对象 `rsrc_windows.syso`（此前需要 rsrc/goversioninfo 这类外部工具），因此 exe 的图标可随项目一起重建。

构建产物新增 `-trimpath`：二进制里不再嵌入开发机的绝对源码路径（此前会泄漏构建者的用户名与目录结构）。

## v0.5.0

新增：

- **`proxy_buffering` / `proxy_buffers` / `proxy_max_temp_file_size`** —— 补齐 nginx 最后一块实质性缺失的机制：缓冲后端响应，让慢客户端不再占住上游连接。此前 crest 只做直通转发，一个慢速客户端下载大文件会把后端连接一直占着（实测 4MB 响应、12MB/s 客户端，后端被占用 ~150ms）；开启缓冲后同样场景后端占用降到 **1–9ms**。默认开启，与 nginx 一致，http/server/location 就近覆盖。

  两处刻意与 nginx 不同：**（1）有数据立即转发**，不等缓冲区填满，所以 SSE、长轮询等增量响应在缓冲开启状态下延迟不变（nginx 这种场景必须手动关缓冲）；**（2）转存临时文件在独立 goroutine 上异步进行**，不占用上游的关键路径 —— 早期同步写盘的版本实测和不缓冲一样慢（50–183ms），改异步后才降到个位数毫秒。

  `proxy_buffers: {count, size}` 设定单个响应的内存预算（默认 8×32k），超出异步落盘；`proxy_max_temp_file_size: 0` 表示只用内存、不落盘（超出即退化为直通背压）；`proxy_buffering: false` 完全关闭。`proxy_buffer_size` / `proxy_busy_buffers_size` / `proxy_request_buffering` / `proxy_temp_file_write_size` 在 crest 中无对应机制，静默接受不再告警。

## v0.4.7

新增：

- **管理页日志区新增「清空」按钮** —— 一键清空当前查看的 `success.log` / `error.log` / `console.log`（带二次确认）。写入方以 `O_APPEND` 持有文件，故采用原地截断，无需重开文件、也不会留下空洞；对应接口 `POST /api/logs/clear?name=<success|error|console>`，同样受令牌与 `ui.allow` 保护。

安全修复：

- **管理台保存配置时生成的 `.bak` 备份是全局可读的**。备份是配置文件的完整副本，通常含 `ui.token`（管理令牌）。控制文件 `.ctl` 一直很谨慎地用 0600，备份却固定用 0644 —— Linux 上任何本地用户都能从 `crest.yml.bak` 读到管理令牌。现在备份沿用原配置文件的权限位（取不到则退回 0600）。
- **Windows 上 `..\` 可穿越 `root` 目录**。URI 里的反斜杠对 `path.Clean` 是普通字符，但 `filepath.Join` 在 Windows 上会把它当作真正的路径分隔符并处理 `..`，于是形如 `/..%5C..%5Ccrest.yml` 的请求可以读到 root 之外的文件（包括含管理令牌的配置文件本身）。Linux 不受影响（反斜杠是合法文件名字符）。现在映射前先将反斜杠归一为 URI 分隔符，并对结果做一次「是否仍在 base 目录内」的兜底校验；两条防线在所有平台生效。浏览器本就会按 WHATWG URL 规范把 `\` 转成 `/`，故正常访问不受影响。

修复：

- **WebSocket 经 `upstream` 负载均衡池时必定 502**。走 upstream 时响应体会被包一层，以便连接关闭时把后端归还给连接池（least_conn 计数需要），但这层包装只保留了 `io.ReadCloser`，丢掉了 `Write`。而 Go 的反向代理在处理 101 时要把响应体断言成 `io.ReadWriteCloser` 来搭建双向隧道，断言失败即报 `101 switching protocols response with non-writable body` 并返回 502。直连 `proxy_pass http://host:port` 不受影响，只有写成 `proxy_pass http://<upstream名>` 才会触发。包装层现在透传 `Write`，归还逻辑保持不变。
- **`error_page` 拦截后，被拦截响应的响应头会串到自定义错误页上**。502 自带的 `Content-Type: text/plain` 会盖住错误页的真实类型，导致自定义的 HTML 错误页在浏览器里**显示成源码**；内部拦截标记 `X-Crest-Internal` 也会一并泄漏给客户端。现在拦截时清空已暂存的响应头，让错误页像 nginx 那样以全新响应开始。
- **只有反代 location、没有 `root` 的 server，未匹配请求返回 500 而非 404**。这类「纯反代虚拟主机」很常见（如只挂了几个 `proxy_pass` 的后台端口），此前每个走丢的 `/favicon.ico`、每次扫描器探测 `/` 都会被记成 500，既不符合 nginx 行为（nginx 用内置默认 root 兜底并返回 404），也把 `error.log` 和监控告警搅乱。现在返回 404，并以 warn 级别记录一行提示（默认 error 级别下不输出）。
- **`proxy_cache` 会被 HEAD 请求毒化**。HEAD 与 GET 共用同一缓存键，但 HEAD 没有响应体；先前 HEAD 的空响应会被写入缓存，导致之后所有 GET 都返回 `200` 加**空 body**。现在只有 GET 能写入缓存条目，HEAD 仍可命中 GET 建立的条目。
- **`rewrite` 跳转到绝对 URL 时丢失原始查询串**。nginx 会把原请求的参数追加到替换串上（除非替换串以 `?` 结尾），crest 此前只对本地替换串这么做，绝对 URL 直接原样跳转 —— 于是 `rewrite ^/x/(.*)$ http://host/$1 redirect;` 会把客户端的 `?a=1` 整个丢掉。同时修正了以 `?` 结尾时 `Location` 残留一个裸 `?` 的问题。
- **WebSocket 在配了 `add_header` 或 `expires` 的路由上必定 502**。这两个指令的响应包装器（`headerAdder` / `expiresWriter`）没有透传 `Hijack()`，导致反代拿不到底层连接、无法完成 101 协议升级，Go 反代直接报 `can't switch protocols using non-Hijacker ResponseWriter type`，最终返回 `502 Bad Gateway`。由于 `add_header` / `expires` 可以在 http / server 层继承下来，只要外层配了一条，该 server 下所有 WebSocket 都会挂 —— 而 location 本身看不出任何问题，排查起来很隐蔽。现已补齐透传，与 `gzip` / `proxy_cache` / `limit_rate` 等其它包装器保持一致。
- **`error_log: {path: stderr}` 会静默吞掉所有运行时错误**。此前只有不写 `error_log` 时错误才进 `log/console.log`，显式写 `stderr` 则直写进程原始 stderr —— 而以 Windows 服务 / 双击 exe / 后台方式启动时那份 stderr 没人接收，`proxy error for ...` 这类关键诊断信息全部丢失，排查 502 时只能看到访问日志里干巴巴的状态码。现在 `stderr`（以及 `/dev/stderr`）与留空一样走 console 汇流，同时输出到终端和 `log/console.log`，与文档中「console.log 记录进程消息和运行时错误」的定义一致。
- 新增 WebSocket 升级回归测试，覆盖裸反代与叠加 `add_header` / `expires` / `gzip` / `error_page` 的组合。

## v0.4.6

- `rewrites` —— 现在既接受单条规则的 map 写法 `rewrites: {pattern: ..., replacement: ..., flag: redirect}`，也接受多条规则的列表写法 `rewrites: [{...}, {...}]`。此前只收列表，单条也得写成列表，容易踩坑。注意：`replacement` 里含 `:` 的 URL（如 `http://$host:9022/...`）在 YAML 流式 `{...}` 中必须加引号。

## v0.4.5

- `proxy_ssl_verify` —— 反代到 HTTPS 上游时是否校验上游证书。**默认 off（对齐 nginx）**：反代自签证书的 HTTPS 后端（如内网两级反代）开箱即用，不再因证书校验失败返回 502。想强制校验可显式 `proxy_ssl_verify: on`（http/server/location 就近覆盖）。

## v0.4.4

性能修复：

- **到后端的空闲连接池默认值 16 → 100**。压测发现旧默认值在中高并发下严重不足：每请求被迫新建/关闭到后端的 TCP 连接（connect/close 系统调用风暴 + TIME_WAIT 堆积），导致吞吐腰斩、长尾延迟飙到秒级。修复后同等压测吞吐翻倍、p100 长尾从 ~2900ms 降到 ~40ms。超高并发场景仍建议用 upstream `keepalive` 显式匹配并发数（不宜盲目配过大）。
- 反代到后端时不再探测 HTTP/2（`ForceAttemptHTTP2: false`），后端为明文 HTTP/1.1 时省去无谓开销。
- 新增诊断特性：环境变量 `CREST_PPROF=127.0.0.1:6060` 启动可开启 Go pprof（默认关闭），方便压测排查。

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

首个正式发布版本。crest 是一个用 Go 编写的 nginx 替代品：单文件、无外部依赖，
配置文件直接兼容 nginx.conf，同时提供更清爽的原生 YAML 格式和内置的浏览器管理控制台。

### 亮点

- **原生 YAML 配置** —— 除 nginx.conf 语法外新增 YAML 格式，按扩展名 / 内容自动识别；
  未知字段在校验时直接报错，避免拼写错误被静默忽略。
- **浏览器管理控制台** —— `crest -s ui` 或配置 `ui` 段一键打开：
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
- 运维命令新增 `crest -s ui`（打开控制台）、`crest -s reopen`（配合日志轮转）。

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
