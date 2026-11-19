package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func autoServer() *Server {
	return &Server{
		ssoURL:       "/_dev/sso",
		publicScheme: "auto",
		schemeAuto:   true,
		// COOKIE_SECURE 没显式设置时，auto 模式跟随协议
		cookieSecureAuto: true,
		cookieSecure:     true,
		cookieKey:        []byte("test-cookie-key"),
		baseDomain:       "dsh.corp",
	}
}

func TestSchemeForAuto(t *testing.T) {
	s := autoServer()

	cases := []struct {
		name   string
		proto  string
		tls    bool
		expect string
	}{
		{"ALB 带 https", "https", false, "https"},
		{"ALB 带 http", "http", false, "http"},
		{"多值时取第一个", "https, http", false, "https"},
		{"大小写与空格", "  HTTPS ", false, "https"},
		{"非法值退回 TLS", "gopher", true, "https"},
		{"没有头但有 TLS", "", true, "https"},
		{"没有头也没有 TLS", "", false, "http"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if c.proto != "" {
			req.Header.Set("X-Forwarded-Proto", c.proto)
		}
		if c.tls {
			req.TLS = &tls.ConnectionState{}
		}
		if got := s.schemeFor(req); got != c.expect {
			t.Errorf("%s: schemeFor = %q, want %q", c.name, got, c.expect)
		}
	}
}

func TestSchemeForExplicitMode(t *testing.T) {
	// 非 auto 模式必须无视请求头：否则任何人都能伪造 X-Forwarded-Proto
	s := &Server{publicScheme: "https"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Proto", "http")
	if got := s.schemeFor(req); got != "https" {
		t.Errorf("显式模式下 schemeFor = %q, want https（不应读请求头）", got)
	}

	// 未配置（或 auto）时按请求判断：http 请求就是 http，绝不升级成 https
	empty := &Server{}
	if got := empty.schemeFor(req); got != "http" {
		t.Errorf("未配置且请求为 http 时应得到 http，得到 %q", got)
	}
}

func TestSecureForFollowsSchemeInAuto(t *testing.T) {
	s := autoServer()
	httpsReq := httptest.NewRequest(http.MethodGet, "/", nil)
	httpsReq.Header.Set("X-Forwarded-Proto", "https")
	httpReq := httptest.NewRequest(http.MethodGet, "/", nil)
	httpReq.Header.Set("X-Forwarded-Proto", "http")

	if !s.secureFor(httpsReq) {
		t.Error("https 下 cookie 应为 Secure")
	}
	if s.secureFor(httpReq) {
		t.Error("http 下 cookie 不应带 Secure（带了浏览器不会回传）")
	}

	// 显式配置时不受请求影响
	fixed := &Server{cookieSecure: true}
	if !fixed.secureFor(httpReq) {
		t.Error("显式 COOKIE_SECURE=true 应始终 Secure")
	}
}

func TestRedirectToSSOUsesForwardedProto(t *testing.T) {
	s := autoServer()
	s.devSubject = "alice@corp"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "alice.dsh.corp"
	req.Header.Set("X-Forwarded-Proto", "https")
	s.redirectToSSO(rec, req, "/")

	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("bad Location: %v", err)
	}
	ret := loc.Query().Get("return")
	if want := "https://alice.dsh.corp/_auth/callback"; ret != want {
		t.Errorf("return = %q, want %q", ret, want)
	}

	// 同样的请求但 ALB 转发的是 http
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Host = "alice.dsh.corp"
	req2.Header.Set("X-Forwarded-Proto", "http")
	s.redirectToSSO(rec2, req2, "/")

	loc2, _ := url.Parse(rec2.Header().Get("Location"))
	if ret := loc2.Query().Get("return"); ret != "http://alice.dsh.corp/_auth/callback" {
		t.Errorf("http 场景下 return = %q", ret)
	}
}

func TestSessionCookieSecureInAuto(t *testing.T) {
	s := autoServer()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Proto", "http")

	c := s.newSessionCookie("alice@corp", time.Now().Add(time.Hour))
	c.Secure = s.secureFor(req)
	if c.Secure {
		t.Error("http 下会话 cookie 不应带 Secure")
	}
}
