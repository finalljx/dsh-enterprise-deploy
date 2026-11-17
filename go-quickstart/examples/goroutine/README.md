# goroutine 示例集

9 个可运行的并发场景，每个都对应真实工程问题。

```bash
go run ./examples/goroutine        # 或 make examples
go run -race ./examples/goroutine  # 带竞态检测（并发代码必跑）
```

## 场景速查

| # | 场景 | 模式 | 关键 API |
|---|---|---|---|
| 1 | 并行调用多个下游服务 | fan-out + `WaitGroup` | `go` + `wg.Add/Done/Wait` |
| 2 | 收集多个并发结果 | channel 收集 | 带缓冲 `make(chan T, n)` |
| 3 | 限制并发度处理大量任务 | **Worker Pool** | `chan` + `close` + `range` |
| 4 | 超时 / 用户取消 | `context` 取消传播 | `WithTimeout` + `ctx.Done()` |
| 5 | 流式处理（读大文件、ETL） | **Pipeline** | `<-chan T` 方向类型 |
| 6 | 取最快返回（主备/缓存+DB） | `select` 多路等待 | `select` + `time.After` |
| 7 | 后台定时任务（清理/上报） | `Ticker` + ctx 退出 | `NewTicker` + `defer Stop()` |
| 8 | 懒加载单例、一次性初始化 | `sync.Once` | `once.Do(fn)` |
| 9 | 任一失败即取消其余 | `errgroup` 手写版 | `WithCancel` + `Once` |

## 实测耗时（本机）

| 场景 | 串行 / 朴素做法 | 并发做法 |
|---|---|---|
| 3 个 300ms 的下游调用 | 900ms | **300ms** |
| 9 个 100ms 任务 / 3 worker | ~900ms（串行） | **303ms** |
| 2 个请求取最快 | 130ms（等最慢） | **81ms** |

## 五个必须避开的坑

### ① goroutine 泄漏

```go
// ❌ 没有退出路径：ctx 超时后这个 goroutine 永远卡在发送上
go func() { ch <- slowCall() }()

// ✅ 要么 select ctx.Done()，要么给 channel 缓冲
go func() {
    select {
    case ch <- slowCall():
    case <-ctx.Done():
    }
}()
```

**判断标准**：每个 `go` 都必须能回答"它什么时候、因为什么条件退出"。

### ② `WaitGroup.Add` 的位置

```go
for i := 0; i < n; i++ {
    go func() { defer wg.Done(); ... }()  // ❌ 先 go 后 Add，可能 Wait 提前返回
    wg.Add(1)
}

for i := 0; i < n; i++ {
    wg.Add(1)                              // ✅ 必须在 go 之前
    go func() { defer wg.Done(); ... }()
}
```

### ③ 忘了 `close(jobs)`

`for j := range jobs` 只有在 channel **被关闭**时才结束。不 close 就是永久阻塞。
但**只能由发送方 close**，接收方 close 会 panic。

### ④ 无缓冲 channel 的单人舞

无缓冲 channel 要求收发双方同时就绪。只有一个 goroutine 时，`ch <- x` 会永久阻塞。
"收集 N 个结果"用 `make(chan T, N)` 最省心。

### ⑤ `time.Tick` 泄漏

```go
ticker := time.Tick(time.Second)   // ❌ 无法停止，永久占着一个定时器
ticker := time.NewTicker(time.Second)
defer ticker.Stop()                // ✅
```

同理，`context.WithTimeout` / `WithCancel` 的 `cancel` 必须调用（`defer cancel()`），
否则 `go vet` 会警告 `lostcancel`。

## 和 JS / Java 对照

| 需求 | JS | Java | Go |
|---|---|---|---|
| 起并发任务 | `async fn()` | `executor.submit()` | `go fn()` |
| 等全部完成 | `await Promise.all` | `CompletableFuture.allOf` | `sync.WaitGroup` |
| 取最快 | `Promise.race` | `invokeAny` | `select` |
| 取消 | `AbortController` | `Future.cancel` | `context.Context` |
| 限流并发 | `p-limit` 库 | 线程池大小 | Worker Pool（自带） |
| 互斥 | 单线程，无需 | `synchronized` | `sync.Mutex` |

## 关于 `errgroup`

第 9 个例子是 `golang.org/x/sync/errgroup` 的手写等价物。生产代码直接用库更省事：

```go
g, ctx := errgroup.WithContext(ctx)
for _, t := range tasks {
    g.Go(func() error { return do(ctx, t) })   // 任一返回 error 即取消其余
}
if err := g.Wait(); err != nil { ... }
```
