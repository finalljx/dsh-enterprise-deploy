# Go 快速上手 Demo

一个**只用标准库**、可直接运行的小型 HTTP 服务。目的不是"功能多"，而是把 Go 日常开发中
80% 的套路（分层、错误处理、中间件、并发安全、测试、优雅退出）一次性演示完。

> 给有 JavaScript / Java 经验的开发者。每一步都标注了"这对应你熟悉的什么"。

---

## 1. 跑起来（30 秒）

```bash
cd go-quickstart
go run ./cmd/api          # 默认监听 :8080
```

另开一个终端：

```bash
curl localhost:8080/healthz
curl localhost:8080/api/users
```

或者用 Makefile（`make` 后面可以补 `help` 看全部命令）：

```bash
make run      # 启动
make test     # 跑测试
make build    # 编译成 bin/api
```

`Ctrl-C` 会触发优雅退出（最多等 10 秒把在途请求处理完），日志里能看到
`signal received, shutting down gracefully`。

---

## 2. 目录结构：Go 项目长什么样

```
go-quickstart/
├── go.mod                       # 模块定义（≈ package.json / pom.xml）
├── Makefile                     # 命令入口
├── cmd/
│   └── api/
│       └── main.go              # 唯一入口：读配置 -> 装配依赖 -> 启服务 -> 等信号
├── examples/
│   ├── goroutine/main.go        # 9 个并发场景（go run ./examples/goroutine）
│   └── closures/                # Go 闭包 vs JS 闭包（main.go + demo.mjs）
└── internal/                    # 只有本模块能 import，编译器强制（比 Java 包可见性好用）
    ├── config/config.go         # 环境变量 -> 结构体
    ├── user/user.go             # 领域模型 + 校验 + 自定义错误类型
    ├── store/
    │   ├── store.go             # Store 接口 + 哨兵错误
    │   ├── memory.go            # 内存实现（线程安全）
    │   └── memory_test.go
    └── httpapi/
        ├── router.go            # 路由表 + 中间件装配
        ├── handlers.go          # 业务 handler + 错误到状态码的映射
        ├── middleware.go        # requestID / accessLog / recoverer
        ├── json.go              # JSON 编解码收口
        └── handlers_test.go     # 端到端测试（httptest 起真服务）
```

三条 Go 的硬性规则，先记住能少踩坑：

1. **一个目录 = 一个包**，同包文件互相无条件可见，不用 import。
2. **首字母大写 = 导出（public）**，小写 = 包内私有。没有 `public` / `private` 关键字。
3. **`internal/` 目录外部不可 import**，是编译器强制的封装边界。

---

## 3. 接口一览

| 方法 | 路径 | 说明 | 成功 |
|---|---|---|---|
| GET | `/healthz` | 健康检查 | 200 |
| GET | `/api/users` | 列表 | 200 |
| POST | `/api/users` | 创建 | 201 |
| GET | `/api/users/{id}` | 详情 | 200 |
| PATCH | `/api/users/{id}` | 部分更新 | 200 |
| DELETE | `/api/users/{id}` | 删除 | 204 |

错误码约定：

| 状态码 | `error` 字段 | 场景 |
|---|---|---|
| 400 | `invalid_json` / `invalid_id` | body 不是合法 JSON、路径参数不是正整数 |
| 404 | `not_found` | 用户不存在 |
| 405 | — | 路径存在但方法不对（标准库自动处理） |
| 409 | `email_taken` | 邮箱重复 |
| 422 | `validation_failed` | 字段校验失败，`fields` 里有逐字段原因 |
| 500 | `internal_error` | 未知错误（详情只进日志，不回给客户端） |

### 完整演示

```bash
# 列表（启动时会种两条数据）
curl -s localhost:8080/api/users | jq

# 创建
curl -s -X POST localhost:8080/api/users \
  -H 'Content-Type: application/json' \
  -d '{"name":"Carol","email":"carol@example.com"}'
# -> {"id":3,"name":"Carol","email":"carol@example.com","created_at":"..."}

# 部分更新：只改 name，email 保持不变
curl -s -X PATCH localhost:8080/api/users/3 \
  -H 'Content-Type: application/json' \
  -d '{"name":"Carol2"}'

# 校验失败 -> 422 + 逐字段原因
curl -s -X POST localhost:8080/api/users \
  -H 'Content-Type: application/json' \
  -d '{"name":"","email":"nope"}'
# -> {"error":"validation_failed","message":"...","fields":{"email":"格式不正确","name":"不能为空"}}

# 邮箱冲突 -> 409
curl -s -X POST localhost:8080/api/users \
  -H 'Content-Type: application/json' \
  -d '{"name":"X","email":"carol@example.com"}'

# 删除 -> 204，再查 -> 404
curl -s -X DELETE -o /dev/null -w '%{http_code}\n' localhost:8080/api/users/3
curl -s localhost:8080/api/users/3
```

---

## 4. 每个概念落在哪一行

| 概念 | 文件 | 给你的对照 |
|---|---|---|
| `main` 只做装配 | `cmd/api/main.go` | 相当于 Spring 的 `@Configuration` + `main`，但全手写显式 |
| 优雅退出 | `main.go` 的 `signal.NotifyContext` + `srv.Shutdown` | JS 的 `process.on('SIGTERM')`，但能把取消信号沿 ctx 传下去 |
| 结构体 + 方法 | `internal/user/user.go` | 相当于 class 的字段 + 方法，但没有继承 |
| struct tag | 字段后的 `` `json:"id"` `` | ≈ `@JsonProperty`，但只是元信息 |
| 指针字段表示"可选" | `UpdateInput` | Go 没有 `undefined`，用 `*string` + `nil` |
| 自定义错误类型 | `ValidationError` | ≈ 自定义 Exception，但用 `errors.As` 取回 |
| **隐式接口** | `store/store.go` | **Go 最核心的差异**：`Memory` 不写 `implements` 也满足 `Store` |
| 编译期接口断言 | `var _ Store = (*Memory)(nil)` | 改接口签名时立刻编译失败 |
| 哨兵错误 | `ErrNotFound` / `ErrEmailTaken` | ≈ 枚举型错误码，用 `errors.Is` 判断 |
| `context.Context` | 所有 store 方法签名 | ≈ `AbortSignal` / 线程中断，但必须显式传 |
| `sync.RWMutex` | `memory.go` | ≈ `ReentrantReadWriteLock`，**不能拷贝** |
| 值语义 / 拷贝 | `memory.go` 的 `clone()` | JS 里对象是引用，Go 的 struct 赋值是深拷贝 |
| `defer` | `defer m.mu.Unlock()` | ≈ `try-with-resources`，但它是函数级、LIFO |
| 中间件 | `middleware.go` | `func(http.Handler) http.Handler`，Go 里没有装饰器语法糖 |
| 显式构造超时 | `main.go` 的 `http.Server` 字段 | 生产必备，Go 默认**没有**读写超时 |
| 标准库路由 | `router.go` 的 `"GET /api/users/{id}"` | Go 1.22+ 内置，很多小项目不需要 gin |
| 表驱动测试 | `memory_test.go` | ≈ `@ParameterizedTest`，但就是一个切片 + 循环 |
| 端到端测试 | `handlers_test.go` 的 `httptest` | ≈ Spring 的 `MockMvc`，起真端口 |

---

## 5. 三个高频"思维转换"

### ① 错误是值，必须显式处理

```go
u, err := h.store.Get(r.Context(), id)
if err != nil {
    h.fail(w, r, err)   // 统一翻译成 HTTP 状态码
    return
}
```

没有 `try/catch`。**每个可能失败的操作后面都可以跟 `if err != nil`**，这是 Go 代码里最常见的三行。
别嫌烦——它在编译期就逼你面对错误。

`panic` 只用于真正的 bug，由 `middleware.go` 的 `recoverer` 在边界统一兜住。

### ② 依赖靠传参，不靠全局单例

```go
type handlers struct {
    store  store.Store      // 接口，不是具体类型
    logger *slog.Logger
}

func NewRouter(s store.Store, logger *slog.Logger) http.Handler { ... }
```

换成 PostgreSQL 时，只要新写一个实现 `Store` 的结构体，`NewRouter` 那行不用改。
没有 Spring 容器，没有 `@Autowired`——**显式传参就是 Go 的依赖注入**。

### ③ 并发安全要么加锁，要么用 channel

`memory.go` 用 `sync.RWMutex` 保护 map：**Go 的 map 并发读写会直接 crash**（不是抛异常，是 fatal）。
`defer m.mu.Unlock()` 保证 panic 时也能解锁。

---

## 6. 接下来按这个顺序改，学习效果最好

1. **加一个字段**：给 `User` 加 `Age int`，走一遍 领域 → 校验 → store → 测试。感受"改一处编译就告诉你还要改哪"。
2. **换持久层**：把 `memory.go` 换成 `sqlite.go` / `postgres.go`（`database/sql` + `sqlx`），`httpapi` 一行不改。
3. **加分页**：`GET /api/users?limit=10&offset=0`，练习 `r.URL.Query()` 和 `strconv`。
4. **加中间件**：写一个简单的 Bearer Token 鉴权，体会 `chain()` 的组合能力。
5. **换成 `errgroup`**：并发查多个下游服务，用 `golang.org/x/sync/errgroup` 替代手写 `WaitGroup` + 错误收集。

## 7. 常用命令

```bash
go run ./cmd/api            # 编译并运行
go build ./...              # 编译所有包
go test ./...               # 跑测试
go test -race ./...         # 带竞态检测（并发代码必跑）
go test -cover ./...        # 看覆盖率
go vet ./...                # 静态检查
gofmt -l .                  # 列出未格式化的文件（空 = 都格式化好了）
go doc net/http.Server      # 查标准库文档
```

> 编辑器里务必打开 **保存时自动 gofmt**。Go 没有风格之争，因为只有一种格式。
