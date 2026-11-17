// 结构化日志与请求作用域。
//
// 两条约定，其它文件都依赖它们：
//   - 输出统一走 slog（JSON），级别由 LOG_LEVEL 控制，默认 info；
//   - 每次调用带一个 request id，从 HTTP 入口一直传到 k8s 请求日志，
//     这样「管理台点一次开通」和它的五个阶段能用同一个 req 串起来。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

const requestIDHeader = "X-Request-ID"

type ctxKey int

const ctxKeyRequestID ctxKey = iota

// setupLogging 安装进程级默认 logger。
func setupLogging() *slog.Logger {
	level, ok := parseLevel(os.Getenv("LOG_LEVEL"))
	if !ok {
		// 拼错的级别不能静默变成 info，否则排查时会以为打开了 debug。
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).
			Warn("unknown LOG_LEVEL, falling back to info", "value", os.Getenv("LOG_LEVEL"))
	}
	l := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(l)
	return l
}

func parseLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, true
	case "", "info":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return slog.LevelInfo, false
}

// withRequestID 复用上游带来的 X-Request-ID，没有就生成一个，并回写响应头。
// 超长或含异常字符的值一律丢弃：它会进日志，也可能进审计。
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(requestIDHeader))
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, id)))
	})
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

func requestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

// logFor 返回带请求作用域的 logger；没经过中间件时退化为默认 logger。
func logFor(ctx context.Context) *slog.Logger {
	if id := requestID(ctx); id != "" {
		return slog.Default().With("req", id)
	}
	return slog.Default()
}

// fatal 用于启动阶段的不可恢复错误（缺环境变量、连不上库）。
func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}
