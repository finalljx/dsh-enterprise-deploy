package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func devSSOServer() *Server {
	return &Server{
		ssoURL:       "/_dev/sso",
		devSubject:   "alice@corp",
		cookieKey:    []byte("test-cookie-key"),
		publicScheme: "http",
		baseDomain:   "dsh.corp",
	}
}

func TestDevSubjectForHost(t *testing.T) {
	s := devSSOServer()

	if got := s.devSubjectForHost("usera.dsh.corp"); got != "usera@corp" {
		t.Errorf("usera.dsh.corp -> %q, want usera@corp", got)
	}
	if got := s.devSubjectForHost("alice.dsh.corp"); got != "alice@corp" {
		t.Errorf("alice.dsh.corp -> %q, want alice@corp", got)
	}

	// 生产（没设 DEV_SUBJECT）时不能从 Host 推身份
	prod := devSSOServer()
	prod.devSubject = ""
	if got := prod.devSubjectForHost("usera.dsh.corp"); got != "" {
		t.Errorf("生产模式下不应推导身份，得到 %q", got)
	}
}

func TestRedirectToSSOCarriesSubjectInDev(t *testing.T) {
	s := devSSOServer()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "usera.dsh.corp"

	s.redirectToSSO(rec, req, "/")

	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("bad Location: %v", err)
	}
	if got := loc.Query().Get("subject"); got != "usera@corp" {
		t.Errorf("subject = %q, want usera@corp", got)
	}
	if !strings.Contains(loc.Query().Get("return"), "usera.dsh.corp/_auth/callback") {
		t.Errorf("return = %q", loc.Query().Get("return"))
	}
}

func TestDevSSOHonorsSubjectParam(t *testing.T) {
	s := devSSOServer()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/_dev/sso?subject=usera%40corp&return="+url.QueryEscape("http://usera.dsh.corp/_auth/callback"), nil)
	s.handleDevSSO(rec, req)

	loc := rec.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("bad Location %q: %v", loc, err)
	}
	ticket := u.Query().Get("ticket")
	subject, ok := s.verifyTicket(ticket)
	if !ok {
		t.Fatalf("ticket 校验失败: %q", ticket)
	}
	if subject != "usera@corp" {
		t.Errorf("subject = %q, want usera@corp", subject)
	}
}

func TestDevSSORejectsBadSubject(t *testing.T) {
	s := devSSOServer()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/_dev/sso?subject=not-an-email&return="+url.QueryEscape("http://x/_auth/callback"), nil)
	s.handleDevSSO(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rec.Code)
	}
}

// 默认（不带 subject）仍然签发 DEV_SUBJECT，保持原有行为。
func TestDevSSODefaultsToDevSubject(t *testing.T) {
	s := devSSOServer()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/_dev/sso?return="+url.QueryEscape("http://alice.dsh.corp/_auth/callback"), nil)
	s.handleDevSSO(rec, req)

	u, _ := url.Parse(rec.Header().Get("Location"))
	subject, ok := s.verifyTicket(u.Query().Get("ticket"))
	if !ok || subject != "alice@corp" {
		t.Fatalf("subject = %q ok=%v, want alice@corp", subject, ok)
	}
}

func TestSessionCookieRoundTripForOtherUser(t *testing.T) {
	// 签发 usera 的 cookie 后，session() 应认出 usera —— 这是多用户本地验证的关键。
	s := devSSOServer()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "usera.dsh.corp"
	http.SetCookie(rec, s.newSessionCookie("usera@corp", time.Now().Add(time.Hour)))

	next := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		next.AddCookie(c)
	}
	subject, ok := s.session(next)
	if !ok || subject != "usera@corp" {
		t.Fatalf("session = %q ok=%v, want usera@corp", subject, ok)
	}
}
