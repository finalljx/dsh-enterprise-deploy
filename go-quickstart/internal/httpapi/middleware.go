package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// middleware 是"接收 handler，返回 handler"的函数。
// Go 没有装饰器，中间件就是一个普通函数——这是 Go 里最典型的函数式写法。
type middleware func(http.Handler) http.Handler

// chain 把中间件按声明顺序套在 handler 外层：
// chain(h, a, b, c) 的执行顺序是 a -> b -> c -> h。
func chain(h http.Handler, ms ...middleware) http.Handler {
	for i := len(ms) - 1; i >= 0; i-- {
		h = ms[i](h)
	}
	return h
}

// 用自定义类型做 context key，避免和其他包的 key 冲突。
type ctxKey string

const ctxKeyRequestID ctxKey = "request_id"

// RequestIDFrom 从 ctx 里取请求 ID（取不到返回空串）。
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

// requestID 给每个请求打一个 ID：优先复用上游传来的，否则自己生成。
// 会顺手回写到响应头，方便前端和日志对齐。
func requestID() middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-Id")
			if id == "" {
				var b [8]byte
				_, _ = rand.Read(b[:])
				id = hex.EncodeToString(b[:])
			}
			w.Header().Set("X-Request-Id", id)

			// context 是只读的，派生新 ctx 再往下传
			ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// statusWriter 包一层 ResponseWriter，用来记录状态码和响应字节数。
// 这就是"隐性接口"的实战：http.ResponseWriter 只要实现 Header/Write/WriteHeader，
// 我们包一层增强后依然满足它。
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap 让 http.ResponseController（流式响应、超时控制）能拿到底层 Writer。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// accessLog 打印一行结构化访问日志（JSON 格式，方便接 Loki/ELK）。
func accessLog(logger *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}

			next.ServeHTTP(sw, r)

			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			logger.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"bytes", sw.bytes,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", RequestIDFrom(r.Context()),
			)
		})
	}
}

// recoverer 兜住 panic，保证单个请求出错不会带走整个进程。
//
// Go 没有 try/catch：panic 是"程序 bug"级别的错误。
// 在 HTTP 服务里，最外层 recover 一次是标准做法（gin/echo 内部都这么做）。
func recoverer(logger *slog.Logger) middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				logger.Error("panic recovered",
					"panic", rec,
					"path", r.URL.Path,
					"request_id", RequestIDFrom(r.Context()),
					"stack", string(debug.Stack()),
				)
				// 如果响应已经开始写了，这里再写会被忽略，属于可接受的降级
				writeError(w, http.StatusInternalServerError, "internal_error", "服务器内部错误")
			}()

			next.ServeHTTP(w, r)
		})
	}
}
