// dsh-router: 把已验证的身份路由到正确的 pod（参考实现）
//
// 顺序不能变：
//  0. 没有会话：浏览器导航先给登录页，其余客户端直跳 SSO
//  1. 从 HMAC 会话 cookie 取 subject
//  2. 向 control 查 subject -> 目标；未就绪给等待页（接口客户端给 503）
//  3. 校验请求 Host 与目标的 publicHost 一致，不一致 403
//  4. mTLS 连 pod，校验对端证书的 URI SAN
//  5. 网关侧持有 DSH cookie，代理请求（含 WebSocket）
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Record struct {
	Subject      string `json:"subject"`
	PublicHost   string `json:"publicHost"`
	PodIP        string `json:"podIP"`
	PrincipalURI string `json:"principalUri"`
	LaunchToken  string `json:"launchToken"`
	DesiredState string `json:"desiredState"`
}

type Server struct {
	controlURL    string
	routerToken   string
	ssoURL        string
	cookieKey     []byte
	rootCAs       *x509.CertPool
	clientCert    tls.Certificate
	podPort       string
	publicScheme  string
	devSubject    string
	transport     sync.Map // podIP -> *http.Transport
	dshCookies    sync.Map // subject -> string
	cookieSecure  bool
	brand         string
	loginMode     string
	wakeMu        sync.Mutex
	wakeAt        map[string]time.Time
	adminToken    string
	adminSubjects map[string]bool
	baseDomain    string
}

func main() {
	logger := setupLogging()

	s := &Server{
		controlURL:    mustEnv("CONTROL_URL"),
		routerToken:   mustEnv("ROUTER_TOKEN"),
		ssoURL:        mustEnv("SSO_URL"),
		cookieKey:     []byte(mustEnv("COOKIE_KEY")),
		podPort:       envOr("POD_PORT", "8443"),
		publicScheme:  envOr("PUBLIC_SCHEME", "https"),
		devSubject:    os.Getenv("DEV_SUBJECT"), // 本地开发：非空则启用 /_dev/sso
		cookieSecure:  envOr("COOKIE_SECURE", "true") == "true",
		brand:         envOr("DSH_BRAND", "DSH 工作区"),
		loginMode:     envOr("LOGIN_MODE", "page"), // page=先给登录页；direct=直接跳 SSO
		wakeAt:        make(map[string]time.Time),
		adminToken:    os.Getenv("ADMIN_TOKEN"),
		adminSubjects: parseSubjects(os.Getenv("ADMIN_SUBJECTS")),
		baseDomain:    envOr("DSH_BASE_DOMAIN", "dsh.corp"),
	}
	pem, err := os.ReadFile(mustEnv("CA_FILE"))
	if err != nil {
		fatal("read CA failed", "err", err.Error())
	}
	s.rootCAs = x509.NewCertPool()
	if !s.rootCAs.AppendCertsFromPEM(pem) {
		fatal("CA file has no certificate")
	}
	s.clientCert, err = tls.LoadX509KeyPair(mustEnv("CLIENT_CERT"), mustEnv("CLIENT_KEY"))
	if err != nil {
		fatal("load client cert failed", "err", err.Error())
	}

	addr := ":" + envOr("PORT", "8080")
	srv := &http.Server{Addr: addr, Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	logger.Info("dsh-router listening", "addr", addr, "logLevel", os.Getenv("LOG_LEVEL"))
	if err := srv.ListenAndServe(); err != nil {
		fatal("server stopped", "err", err.Error())
	}
}

// routes 是唯一的路由表，main 与测试共用，避免注册与测试漂移。
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/_auth/callback", s.handleCallback)
	mux.HandleFunc("/_dsh/login", s.handleLogin)
	mux.HandleFunc("/_dsh/start", s.handleAuthStart)
	mux.HandleFunc("/_dsh/logout", s.handleLogout)
	mux.HandleFunc("/_dsh/assets/", s.handleAssets)
	mux.HandleFunc("/_dsh/status", s.handleStatus)
	mux.HandleFunc(adminPath, s.handleAdminPage)
	mux.HandleFunc(adminPath+"/api/users", s.handleAdminUsers)
	mux.HandleFunc(adminPath+"/api/desired", s.handleAdminDesired)
	if s.devSubject != "" {
		mux.HandleFunc("/_dev/sso", s.handleDevSSO)
	}
	mux.HandleFunc("/", s.handleRoute)
	return withRequestID(mux)
}

// handleCallback: SSO 回调。ticket 由 SSO 服务用同一密钥签名，格式 subject|exp|hex(hmac)。
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	subject, ok := s.verifyTicket(r.URL.Query().Get("ticket"))
	if !ok {
		http.Error(w, "invalid ticket", http.StatusForbidden)
		return
	}
	next := s.takeNext(w, r)
	http.SetCookie(w, s.newSessionCookie(subject, time.Now().Add(12*time.Hour)))
	http.Redirect(w, r, next, http.StatusFound)
}

// handleDevSSO 只在 DEV_SUBJECT 非空时挂载：本地没有 IdP，直接签发 ticket，
// 但 ticket 的签名与校验走的是和正式流程完全相同的代码路径。
// devSubjectPattern 限制本地假 IdP 能签发的身份形态。
var devSubjectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._%+-]{0,63}@[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

// devSubjectForHost 把子域映射成身份：usera.dsh.corp -> usera@corp。
// 域后缀沿用 DEV_SUBJECT（本地是 corp），只有 dev 模式才返回非空。
func (s *Server) devSubjectForHost(host string) string {
	if s.devSubject == "" {
		return ""
	}
	at := strings.LastIndex(s.devSubject, "@")
	if at < 0 {
		return ""
	}
	slug := strings.SplitN(host, ".", 2)[0]
	if slug == "" {
		return ""
	}
	subj := strings.ToLower(slug) + s.devSubject[at:]
	if !devSubjectPattern.MatchString(subj) {
		return ""
	}
	return subj
}

func (s *Server) handleDevSSO(w http.ResponseWriter, r *http.Request) {
	ret := r.URL.Query().Get("return")
	if ret == "" {
		http.Error(w, "missing return", http.StatusBadRequest)
		return
	}
	// 默认签发 DEV_SUBJECT，可用 ?subject= 指定别人（本地验多用户）
	subject := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("subject")))
	if subject == "" {
		subject = strings.ToLower(s.devSubject)
	}
	if !devSubjectPattern.MatchString(subject) {
		http.Error(w, "bad subject", http.StatusBadRequest)
		return
	}
	exp := time.Now().Add(60 * time.Second).Unix()
	e := fmt.Sprint(exp)
	ticket := subject + "|" + e + "|" + s.mac("ticket|"+subject+"|"+e)
	u, err := url.Parse(ret)
	if err != nil {
		http.Error(w, "bad return", http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("ticket", ticket)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (s *Server) handleRoute(w http.ResponseWriter, r *http.Request) {
	subject, ok := s.session(r)
	if !ok {
		s.beginLogin(w, r)
		return
	}

	rec, status := s.lookup(r.Context(), subject)
	if status == http.StatusNotFound {
		// 控制面里没有这条记录：可能是刚开通还没登记，也可能根本没开通。
		// 不再直接抛 503，交给等待页说明情况。
		s.serveStarting(w, r, stateNotProvisioned, r.URL.RequestURI())
		return
	}
	if status != http.StatusOK {
		s.serveStarting(w, r, stateError, r.URL.RequestURI())
		return
	}

	// ③ Host 必须与身份一致。比较对象是控制面里的值，不是从 Host 反查出来的值。
	// 先校验再判就绪：否则 Alice 会在 Bob 的「启动中」页面上等一个不属于她的工作区。
	if !sameAuthority(r.Host, rec.PublicHost) {
		logFor(r.Context()).Warn("host mismatch", "alert", true,
			"subject", subject, "host", r.Host, "expected", rec.PublicHost)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	state := stateOf(rec)
	if state != stateReady {
		// 只有休眠需要唤醒；running 但还没上报 podIP 的，等它自己注册。
		if state == stateStarting && rec.DesiredState == "sleeping" {
			s.wakeThrottled(r.Context(), subject)
		}
		s.serveStarting(w, r, state, r.URL.RequestURI())
		return
	}

	cookie, err := s.dshCookie(r.Context(), rec)
	if err != nil {
		logFor(r.Context()).Error("dsh auth failed", "alert", true, "subject", subject, "err", err.Error())
		http.Error(w, "upstream auth failed", http.StatusBadGateway)
		return
	}

	target := rec.PodIP + ":" + s.podPort
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "https"
			req.URL.Host = target
			req.Host = rec.PublicHost
			req.Header.Set("Cookie", cookie) // 替换浏览器 cookie，DSH 只看到网关的
			req.Header.Del("X-Forwarded-User")
		},
		Transport:     s.transportFor(rec),
		FlushInterval: -1, // 流式输出立即 flush
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			logFor(r.Context()).Error("proxy error", "subject", subject, "err", err.Error())
			http.Error(w, "upstream error", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// dshCookie 用 launch token 换 DSH cookie，并按 pod 缓存。
func (s *Server) dshCookie(ctx context.Context, rec Record) (string, error) {
	if v, ok := s.dshCookies.Load(rec.Subject); ok {
		return v.(string), nil
	}
	u := "https://" + rec.PodIP + ":" + s.podPort + "/?token=" + url.QueryEscape(rec.LaunchToken)
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Host = rec.PublicHost
	setRequestID(req, ctx)
	client := &http.Client{
		Transport: s.transportFor(rec),
		Timeout:   10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // 不要跟随 303，我们只要 Set-Cookie
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	for _, c := range resp.Cookies() {
		if resp.StatusCode/100 == 3 {
			v := c.Name + "=" + c.Value
			s.dshCookies.Store(rec.Subject, v)
			return v, nil
		}
	}
	return "", fmt.Errorf("no session cookie in exchange (status %d)", resp.StatusCode)
}

// transportFor 为每个 pod 建一个 mTLS transport，并校验对端身份。
func (s *Server) transportFor(rec Record) *http.Transport {
	if v, ok := s.transport.Load(rec.Subject); ok {
		return v.(*http.Transport)
	}
	expected := rec.PrincipalURI
	t := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:      s.rootCAs,
			Certificates: []tls.Certificate{s.clientCert},
			ServerName:   rec.PublicHost,
			MinVersion:   tls.VersionTLS12,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return fmt.Errorf("no peer certificate")
				}
				for _, u := range cs.PeerCertificates[0].URIs {
					if u.String() == expected {
						return nil
					}
				}
				return fmt.Errorf("peer identity mismatch: want %s", expected)
			},
		},
		ForceAttemptHTTP2: false, // WebSocket 需要 HTTP/1.1
	}
	actual, _ := s.transport.LoadOrStore(rec.Subject, t)
	return actual.(*http.Transport)
}

// wake 把期望状态改为 running，由回收器负责真正拉起。
func (s *Server) wake(ctx context.Context, subject string) {
	body, _ := json.Marshal(map[string]string{"subject": subject, "desiredState": "running"})
	req, _ := http.NewRequest(http.MethodPost, s.controlURL+"/internal/desired", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.routerToken)
	req.Header.Set("content-type", "application/json")
	setRequestID(req, ctx)
	client := &http.Client{Timeout: 3 * time.Second}
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
	}
}

// wakeThrottled 让等待页的轮询不会把控制面打穿：同一用户 5 秒内只唤醒一次。
func (s *Server) wakeThrottled(ctx context.Context, subject string) {
	s.wakeMu.Lock()
	last, seen := s.wakeAt[subject]
	if seen && time.Since(last) < 5*time.Second {
		s.wakeMu.Unlock()
		return
	}
	s.wakeAt[subject] = time.Now()
	s.wakeMu.Unlock()
	s.wake(ctx, subject)
}

func (s *Server) lookup(ctx context.Context, subject string) (Record, int) {
	u := s.controlURL + "/internal/lookup?subject=" + url.QueryEscape(subject)
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Authorization", "Bearer "+s.routerToken)
	setRequestID(req, ctx)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Record{}, http.StatusBadGateway
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return Record{}, http.StatusNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return Record{}, http.StatusBadGateway
	}
	var rec Record
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		return Record{}, http.StatusBadGateway
	}
	return rec, http.StatusOK
}

// newSessionCookie 只在这一处构造会话 cookie，测试与回调共用，避免两套签名逻辑漂移。
func (s *Server) newSessionCookie(subject string, exp time.Time) *http.Cookie {
	e := fmt.Sprint(exp.Unix())
	return &http.Cookie{
		Name:     "dsh_sess",
		Value:    subject + "|" + e + "|" + s.mac(subject+"|"+e),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteLaxMode,
		Expires:  exp,
	}
}

func (s *Server) session(r *http.Request) (string, bool) {
	c, err := r.Cookie("dsh_sess")
	if err != nil {
		return "", false
	}
	parts := strings.Split(c.Value, "|")
	if len(parts) != 3 {
		return "", false
	}
	subject, exp, sig := parts[0], parts[1], parts[2]
	if !hmac.Equal([]byte(sig), []byte(s.mac(subject+"|"+exp))) {
		return "", false
	}
	var e int64
	if _, err := fmt.Sscan(exp, &e); err != nil || time.Now().Unix() > e {
		return "", false
	}
	return subject, true
}

func (s *Server) verifyTicket(t string) (string, bool) {
	parts := strings.Split(t, "|")
	if len(parts) != 3 {
		return "", false
	}
	subject, exp, sig := parts[0], parts[1], parts[2]
	if !hmac.Equal([]byte(sig), []byte(s.mac("ticket|"+subject+"|"+exp))) {
		return "", false
	}
	var e int64
	if _, err := fmt.Sscan(exp, &e); err != nil || time.Now().Unix() > e {
		return "", false
	}
	return subject, true
}

func (s *Server) mac(v string) string {
	m := hmac.New(sha256.New, s.cookieKey)
	m.Write([]byte(v))
	return hex.EncodeToString(m.Sum(nil))
}

// sameAuthority 忽略默认端口差异。
func sameAuthority(a, b string) bool {
	norm := func(v string) string {
		v = strings.ToLower(strings.TrimSpace(v))
		v = strings.TrimSuffix(v, ":443")
		v = strings.TrimSuffix(v, ":80")
		return v
	}
	return norm(a) == norm(b)
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		fatal("missing required env", "key", k)
	}
	return v
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
