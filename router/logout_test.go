package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func logoutRequest(s *Server, origin string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/_dsh/logout", nil)
	req.Host = "alice.dsh.corp"
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	req.AddCookie(s.newSessionCookie("alice@corp", time.Now().Add(time.Hour)))
	return req
}

// Origin: null 意味着文档源不透明（sandboxed iframe、跨源重定向后），必须拒绝；
// 但响应要说清是跨站，而不是让人以为会话失效。
func TestLogoutRejectsNullOrigin(t *testing.T) {
	s := devSSOServer()
	rec := run(s.handleLogout, logoutRequest(s, "null"))

	wantStatus(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "origin mismatch") {
		t.Errorf("403 应说明原因，实际 body=%q", rec.Body.String())
	}
}

// 命令行客户端（curl/脚本）不带 Origin，保持可用。
func TestLogoutAcceptsRequestWithoutOrigin(t *testing.T) {
	s := devSSOServer()
	rec := run(s.handleLogout, logoutRequest(s, ""))
	wantStatus(t, rec, http.StatusSeeOther)
}

func TestLogoutRejectsGet(t *testing.T) {
	s := devSSOServer()
	req := logoutRequest(s, "")
	req.Method = http.MethodGet
	wantStatus(t, run(s.handleLogout, req), http.StatusMethodNotAllowed)
}
