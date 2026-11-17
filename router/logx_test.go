package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// ridRecorder 记录 control 侧收到的 X-Request-ID，用来验证透传。
type ridRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *ridRecorder) add(v string) {
	r.mu.Lock()
	r.seen = append(r.seen, v)
	r.mu.Unlock()
}

func (r *ridRecorder) ids() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func newRidServer(t *testing.T) (*Server, *ridRecorder) {
	t.Helper()
	rec := &ridRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/lookup", func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.Header.Get(requestIDHeader))
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/internal/desired", func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.Header.Get(requestIDHeader))
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/internal/users", func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.Header.Get(requestIDHeader))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Server{
		controlURL:  srv.URL,
		routerToken: "rt",
		adminToken:  "at",
		wakeAt:      map[string]time.Time{},
	}, rec
}

func ctxWithID(id string) context.Context {
	return context.WithValue(context.Background(), ctxKeyRequestID, id)
}

func TestRoutesSetRequestIDHeader(t *testing.T) {
	srv := httptest.NewServer((&Server{}).routes())
	t.Cleanup(srv.Close)

	// 带 id 的请求应该原样回写：用户能把控制台里的 id 直接报给运维。
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
	req.Header.Set(requestIDHeader, "user-report-42")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get(requestIDHeader); got != "user-report-42" {
		t.Errorf("响应头 = %q, want user-report-42", got)
	}

	// 没带就生成一个，不能空着。
	resp2, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if got := resp2.Header.Get(requestIDHeader); got == "" {
		t.Error("未生成 X-Request-ID")
	}
}

func TestLookupPropagatesRequestID(t *testing.T) {
	s, rec := newRidServer(t)
	if _, status := s.lookup(ctxWithID("req-lookup"), "alice@corp"); status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	if ids := rec.ids(); len(ids) != 1 || ids[0] != "req-lookup" {
		t.Errorf("control 收到的 X-Request-ID = %v, want [req-lookup]", ids)
	}
}

func TestWakePropagatesRequestID(t *testing.T) {
	s, rec := newRidServer(t)
	s.wake(ctxWithID("req-wake"), "alice@corp")
	if ids := rec.ids(); len(ids) != 1 || ids[0] != "req-wake" {
		t.Errorf("control 收到的 X-Request-ID = %v, want [req-wake]", ids)
	}
}

func TestControlRequestPropagatesRequestID(t *testing.T) {
	s, rec := newRidServer(t)
	if code, _, err := s.controlRequest(ctxWithID("req-admin"), http.MethodGet, "/internal/users", nil, "ops@corp"); err != nil || code != http.StatusOK {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if ids := rec.ids(); len(ids) != 1 || ids[0] != "req-admin" {
		t.Errorf("control 收到的 X-Request-ID = %v, want [req-admin]", ids)
	}
}

func TestWithoutRequestIDNoHeaderSent(t *testing.T) {
	// 没有作用域 id 时不要塞空头：下游仍能自己生成。
	s, rec := newRidServer(t)
	_, _ = s.lookup(context.Background(), "alice@corp")
	if ids := rec.ids(); len(ids) != 1 || ids[0] != "" {
		t.Errorf("X-Request-ID = %v, want [\"\"]", ids)
	}
}

func TestRouterLogsCarryRequestID(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	logFor(ctxWithID("req-log")).Warn("host mismatch", "alert", true, "subject", "alice@corp")

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("日志不是合法 JSON: %v", err)
	}
	if entry["req"] != "req-log" {
		t.Errorf("req = %v, want req-log", entry["req"])
	}
	if entry["alert"] != true {
		t.Errorf("alert = %v, want true（保留原来 ALERT 的可检索性）", entry["alert"])
	}
}
