package httpapi

import (
	"log/slog"
	"net/http"

	"example.com/goquickstart/internal/store"
)

// NewRouter 组装路由和中间件，返回一个 http.Handler。
//
// 返回接口而不是 *http.ServeMux：调用方只关心"能 ServeHTTP 的东西"，
// 这也是 "accept interfaces, return structs" 的例外——返回标准库接口更通用。
func NewRouter(s store.Store, logger *slog.Logger) http.Handler {
	h := &handlers{store: s, logger: logger}

	mux := http.NewServeMux()

	// Go 1.22 起标准库路由支持 "METHOD /path/{param}"，
	// 很多小项目已经不需要 gin/echo 了。
	// 规则：不匹配方法 → 405；不匹配路径 → 404，都由 ServeMux 自动处理。
	mux.HandleFunc("GET /healthz", h.health)

	mux.HandleFunc("GET /api/users", h.listUsers)
	mux.HandleFunc("POST /api/users", h.createUser)
	mux.HandleFunc("GET /api/users/{id}", h.getUser)
	mux.HandleFunc("PATCH /api/users/{id}", h.updateUser)
	mux.HandleFunc("DELETE /api/users/{id}", h.deleteUser)

	return chain(mux,
		recoverer(logger), // 最外层：兜 panic
		requestID(),       // 次外层：生成 ID 供日志使用
		accessLog(logger), // 最内层：记录耗时和状态码
	)
}
