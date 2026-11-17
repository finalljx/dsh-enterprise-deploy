// 结构化日志与请求作用域。
//
// 与 control 侧同一套约定，目的是让一次浏览器请求能在两个服务之间对上：
// 浏览器 → router（生成或透传 X-Request-ID）→ control（带同一个 id 打日志）。
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

func setupLogging() *slog.Logger {
	level, ok := parseLevel(os.Getenv("LOG_LEVEL"))
	if !ok {
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

// withRequestID 复用上游的 X-Request-ID，没有就生成，并回写响应头。
// 回写很重要：用户在管理台上就能把这个 id 报给运维。
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

func logFor(ctx context.Context) *slog.Logger {
	if id := requestID(ctx); id != "" {
		return slog.Default().With("req", id)
	}
	return slog.Default()
}

// setRequestID 把作用域里的 id 带到出站请求上，让下游能接着打。
func setRequestID(req *http.Request, ctx context.Context) {
	if id := requestID(ctx); id != "" {
		req.Header.Set(requestIDHeader, id)
	}
}

func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}
