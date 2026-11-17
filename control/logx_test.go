package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithRequestIDPassesThrough(t *testing.T) {
	var got string
	h := withRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = requestID(r.Context())
	}))

	req := httptest.NewRequest(http.MethodPost, "/internal/provision", nil)
	req.Header.Set(requestIDHeader, "trace-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got != "trace-123" {
		t.Errorf("ctx request id = %q, want trace-123", got)
	}
	if hdr := rec.Header().Get(requestIDHeader); hdr != "trace-123" {
		t.Errorf("response header = %q, want trace-123", hdr)
	}
}

func TestWithRequestIDGeneratesWhenMissing(t *testing.T) {
	var got string
	h := withRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = requestID(r.Context())
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got == "" {
		t.Fatal("没有生成 request id")
	}
	if got != rec.Header().Get(requestIDHeader) {
		t.Errorf("ctx id %q 与响应头 %q 不一致", got, rec.Header().Get(requestIDHeader))
	}
}

func TestWithRequestIDRejectsUnsafeValues(t *testing.T) {
	// 这个值会进日志、可能进审计，不能让调用方塞换行或超长内容。
	for _, bad := range []string{"a\nb", "with space", "quote\"", string(make([]byte, 65))} {
		var got string
		h := withRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = requestID(r.Context())
		}))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set(requestIDHeader, bad)
		h.ServeHTTP(httptest.NewRecorder(), req)
		if got == bad {
			t.Errorf("非法 request id %q 被原样接受", bad)
		}
		if got == "" {
			t.Error("非法 request id 应被替换成新生成的值")
		}
	}
}

func TestLogForAttachesRequestID(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx := context.WithValue(context.Background(), ctxKeyRequestID, "req-42")
	logFor(ctx).Info("hello")

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("日志不是合法 JSON: %v (%s)", err, buf.String())
	}
	if entry["req"] != "req-42" {
		t.Errorf("req 字段 = %v, want req-42", entry["req"])
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug, "": slog.LevelInfo, "info": slog.LevelInfo,
		"warn": slog.LevelWarn, "ERROR": slog.LevelError,
	}
	for in, want := range cases {
		got, ok := parseLevel(in)
		if !ok || got != want {
			t.Errorf("parseLevel(%q) = %v,%v want %v", in, got, ok, want)
		}
	}
	if _, ok := parseLevel("verbose"); ok {
		t.Error("未知级别应返回 ok=false，避免静默降级")
	}
}
