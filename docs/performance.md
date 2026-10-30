# v1.0.0 并发与内存验证

日期：2026-10-30。v1.0.0 按已验证的 Windows/Linux 发布范围交付，不承诺无限并发、固定 RSS 或未经验证的平台表现。

## 发布结论与已知限制

- Linux ARM64：完整压力矩阵全部零错误；crest 单独配置 cgroup v2 `memory.max=268435456`、`memory.swap.max=0`，所有测试进程 `oom=0`、`oom_kill=0`。
- cgroup `memory.peak`（含页缓存）最高约 **274.6MiB**；大响应触发过 `memory.events.max` 回收。Linux 允许瞬时超限，因此配置 256MiB 不等于每一瞬间都严格不超 256MiB。进程 RSS 的最高采样约 **132.3MiB**。
- macOS：部分 512 突发轮次通过，但冷启动回环连接仍有重置；精确限制客户端连接数后也曾出现。该平台极端突发场景**未宣布通过**。标准库对照也出现过重置，但这不足以证明所有问题都由系统造成。
- Windows、Win7、Linux amd64 完成交叉编译；Windows/Win7 真机性能尚未验证。Win7 使用独立 Go 1.20.14；现代目标使用 Go 1.26.0。
- 未修改宿主机 TCP 参数、未给业务请求增加重试、未把失败改成成功统计。历史失败数据保留在 `performance-results.json`。

## Linux 发布矩阵

Ubuntu 24.04 / Linux 6.8 ARM64，4 vCPU、2GiB VM；压测器和后端在 crest cgroup 之外。crest 使用 `GOMAXPROCS=4`、`GOMEMLIMIT=128MiB`，每场景新进程，零爬坡。除持续负载为 60 秒外，其余发起请求 10 秒并等待排空。

| 场景 | 并发 worker | 请求数 | 错误 | 请求/秒 | P99 ms | RSS 峰值 MiB |
|---|---:|---:|---:|---:|---:|---:|
| proxy-32 | 32 | 134745 | 0 | 13473 | 6 | 16.0 |
| proxy-128 | 128 | 152966 | 0 | 15280 | 24 | 30.4 |
| proxy-512 | 512 | 158270 | 0 | 15808 | 91 | 92.9 |
| tls-128 | 128 | 162060 | 0 | 16196 | 23 | 37.7 |
| gzip-128 | 128 | 58768 | 0 | 5862 | 66 | 60.2 |
| static-256 | 256 | 399086 | 0 | 39898 | 30 | 22.5 |
| cache-hot-256 | 256 | 362535 | 0 | 36224 | 23 | 25.1 |
| cache-cold-64 | 64 | 3689 | 0 | 361 | 580 | 132.3 |
| no-store-64 | 64 | 2335 | 0 | 230 | 2588 | 107.2 |
| slow-64 | 64 | 192 | 0 | 15 | 4326 | 103.1 |
| soak-128 | 128 | 867672 | 0 | 14460 | 27 | 32.7 |

每个响应检查 HTTP 200、完整长度和 SHA-256。小响应 1KiB，大响应 4MiB；慢客户端约 1MiB/s。缓存命中场景预热 8 个 Vary 变体，未命中场景使用唯一 URL。TLS 压测为 HTTP/1.1、自签测试证书，不包括 HTTP/2 压测。RSS 每 100ms 用 `ps` 采样，不包括后端/压测器，也可能漏掉短时尖峰。

本次 Linux 保留未限制客户端连接总数的旧口径作更强对照：TLS 的 128 个 worker 实际峰值为 335 个 TCP 连接。工具现在默认 `MaxConnsPerHost=worker数`，可通过 `-client-max-conns 0` 重现旧口径；这不改变请求 worker 并发数，也不添加重试。数据记录 `ClientDials` 和 `PeakClientConnections`。

## 额外重复与延迟波动

512 并发在同一 Linux 硬限制下额外重复三轮，分别为 88,363 / 6,823 / 86,404 次请求，错误均为 0。三轮 P99 分别为 189 / **4,177** / 379ms，RSS 峰值 106.5 / 114.5 / 96.6MiB，OOM 均为 0。吞吐与尾延迟存在明显波动，本次共享宿主机/虚拟机测试不能当作独占裸机容量定级，也没有证明低延迟 SLO。

## 定位与修复

1. 后端空闲连接池 100 → 512，减少反复建连和关闭；显式 upstream `keepalive` 仍覆盖默认值。每个 Transport 最多同时发起 16 个 TCP 建连，等待受取消及超时约束，不限制已建立的长连接。
2. 每个 HTTP listener 增加 512 容量的接收队列，分离 accept 与 HTTP/TLS 初始化。关闭时释放未分发连接，活跃连接走正常优雅关闭；临时 accept 错误退避恢复。该队列不是无限突发保证。
3. 缓存最多同时录制 4 个响应，每个体上限 8MiB；超预算流式 BYPASS，避免每个并发请求都积攒大响应。不可缓存、SSE、HEAD 未命中也直接转发，保留状态码。
4. 缓存文件改为不可变版本，防止并发替换混合响应；读者退出后清理，正常停机清理本实例文件。保留缓存先发布、后释放响应的顺序。
5. 修复缓冲器等待下一段数据时的忙循环；Vary 按规范化策略及全部同名请求头值区分。
6. 增加取消、预算、完整性、关闭、TLS、缓存并发回归。缓存查找耗时比率测试改为 benchmark；缓冲集成测试使用 32MiB 响应以超过本机内核缓存容量，原断言保留。

## 修复前后对照

以下对照均为 GOMAXPROCS=4、GOMEMLIMIT=128MiB，普通场景发起请求 5 秒；同机前后样本不是通用性能加速承诺。

| 场景 | 原峰值 MiB | 修复后峰值 MiB | 原 RPS | 修复后 RPS |
|---|---:|---:|---:|---:|
| proxy-128 | 36.2 | 40.5 | 508 | 12676 |
| cache-cold-64 | 379.9 | 143.4 | 97 | 130 |
| no-store-64 | 331.2 | 109.3 | 72 | 275 |
| slow-64 | 85.6 | 99.9 | 14 | 15 |


## 复现与验证

```sh
mkdir -p dist/perf
go build -trimpath -o dist/perf/crest ./cmd/crest
go build -o dist/perf/load ./tools/perf
./dist/perf/load -binary dist/perf/crest -procs 4 -ramp 0 -output dist/perf/result.json
go test ./... -count=1
go test -race ./... -count=1
go test ./internal/server -run '^TestProxyCache$|^TestQueuedListener' -count=50
```

Linux 硬限额测试需要独立 Linux cgroup-v2 环境：

```sh
sudo env CREST_PERF_BINARY=/absolute/path/to/crest ./dist/perf/load \
  -binary "$PWD/tools/perf/linux-limit.sh" -procs 4 -ramp 0 \
  -client-max-conns 0 -output /tmp/crest-pressure.json
```

启动器只限制自身进程，留下空 cgroup 供审计 `memory.events`、`memory.peak`。不要在生产主机随意运行此 root 示例。本次使用不挂载工作区的临时 Lima 虚拟机，结束后停机释放资源；源码和 SDK 不被迁移或替换。

Go 1.26.0 与独立 Go 1.20.14 下全量与 race 测试通过；缓存/监听测试重复 50 次通过。构建输出使用 `OUTPUT_DIR=dist/v1.0.0 bash build.sh`，保留根目录原有 exe。

## 容量边界

128MiB 的 Go 软限制不能代替硬 RSS 限额。应另为 OS、内核 socket、页缓存、TLS/gzip、缓存磁盘留余量；本结果不是“256MiB 整机”的保证。用 `limit_conn` 约束活跃 HTTP 请求，超过容量返回 503；它不控制尚未收到 HTTP 请求的空闲连接。

缓存索引仍不持久化，异常退出可能留下缓存文件；FastCGI、stream TLS 终止仍为 TODO。本次没有生产网络、Windows 真机、长时间泄漏认证或无限连接攻击测试。

完整指标摘要与历史失败在 `performance-results.json`，Linux 硬限制证据在 `performance-linux.json`，本机原始 RSS 时间序列保留于 `dist/perf/*.json`。
