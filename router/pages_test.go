package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testRouterToken = "router-token"
	testAdminToken  = "admin-token"
)

type fakeControl struct {
	mu          sync.Mutex
	rec         Record
	code        int
	desired     []string
	lookups     int
	users       []Record
	provisionIn map[string]any
	provisionBy string
	lookupAuth  string
	server      *httptest.Server
}

func newFakeControl(t *testing.T, rec Record, code int) *fakeControl {
	t.Helper()
	f := &fakeControl{rec: rec, code: code}
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/lookup", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testRouterToken {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.mu.Lock()
		f.lookups++
		rec, code := f.rec, f.code
		f.mu.Unlock()
		if code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rec)
	})
	mux.HandleFunc("/internal/users", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		list := append([]Record(nil), f.users...)
		f.lookupAuth = r.Header.Get("Authorization")
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("/internal/provision", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.provisionIn = body
		f.provisionBy = r.Header.Get("X-DSH-Actor")
		f.lookupAuth = r.Header.Get("Authorization")
		f.mu.Unlock()
		subject, _ := body["subject"].(string)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"subject": subject,
			"handle":  "bob",
			"host":    "bob.dsh.corp",
		})
	})
	mux.HandleFunc("/internal/desired", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+testRouterToken && auth != "Bearer "+testAdminToken {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var body struct {
			Subject      string `json:"subject"`
			DesiredState string `json:"desiredState"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.desired = append(f.desired, body.Subject+":"+body.DesiredState)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeControl) desiredCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.desired...)
}

func newTestRouter(ctrl *fakeControl) *Server {
	return &Server{
		controlURL:   ctrl.server.URL,
		routerToken:  testRouterToken,
		ssoURL:       "https://sso.test/login",
		cookieKey:    []byte("test-cookie-key"),
		podPort:      "8443",
		publicScheme: "http",
		brand:        "测试工作区",
		loginMode:    "page",
		wakeAt:       make(map[string]time.Time),
	}
}

func aliceRecord() Record {
	return Record{
		Subject:      "alice@corp",
		PublicHost:   "alice.dsh.corp",
		DesiredState: "running",
	}
}

func browserRequest(s *Server, target string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = "alice.dsh.corp"
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	if s != nil {
		req.AddCookie(s.newSessionCookie("alice@corp", time.Now().Add(time.Hour)))
	}
	return req
}

func cliRequest(s *Server, target string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = "alice.dsh.corp"
	if s != nil {
		req.AddCookie(s.newSessionCookie("alice@corp", time.Now().Add(time.Hour)))
	}
	return req
}

func run(h http.HandlerFunc, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, want, rec.Body.String())
	}
}

func wantContains(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("body does not contain %q: %q", want, rec.Body.String())
	}
}

func TestStateOf(t *testing.T) {
	cases := []struct {
		name string
		rec  Record
		want userState
	}{
		{"archived", Record{DesiredState: "archived", PodIP: "10.0.0.1"}, stateArchived},
		{"sleeping", Record{DesiredState: "sleeping"}, stateStarting},
		{"running without pod", Record{DesiredState: "running"}, stateStarting},
		{"running with pod", Record{DesiredState: "running", PodIP: "10.0.0.1"}, stateReady},
		{"legacy empty state", Record{DesiredState: "", PodIP: "10.0.0.1"}, stateReady},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stateOf(c.rec); got != c.want {
				t.Fatalf("stateOf = %q, want %q", got, c.want)
			}
		})
	}
}

func TestWantsHTML(t *testing.T) {
	cases := []struct {
		name   string
		mode   string
		accept string
		want   bool
	}{
		{"navigation", "navigate", "text/html", true},
		{"navigation wins over accept", "navigate", "application/json", true},
		{"cors fetch", "cors", "text/html", false},
		{"curl", "", "*/*", false},
		{"legacy browser", "", "text/html,application/xhtml+xml", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if c.mode != "" {
				r.Header.Set("Sec-Fetch-Mode", c.mode)
			}
			if c.accept != "" {
				r.Header.Set("Accept", c.accept)
			}
			if got := wantsHTML(r); got != c.want {
				t.Fatalf("wantsHTML = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"":                      "/",
		"/":                     "/",
		"/foo?x=1":              "/foo?x=1",
		"//evil.example":        "/",
		"https://evil.example":  "/",
		"http://evil.example/p": "/",
		"/\\evil.example":       "/",
		"/a\nb":                 "/",
		"relative":              "/",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNoSessionNavigationGoesToLogin(t *testing.T) {
	s := newTestRouter(newFakeControl(t, aliceRecord(), http.StatusOK))
	rec := run(s.handleRoute, browserRequest(nil, "/"))
	wantStatus(t, rec, http.StatusFound)
	if got := rec.Header().Get("Location"); got != "/_dsh/login?next=%2F" {
		t.Fatalf("Location = %q", got)
	}
}

func TestNoSessionCLIGoesToSSO(t *testing.T) {
	s := newTestRouter(newFakeControl(t, aliceRecord(), http.StatusOK))
	rec := run(s.handleRoute, cliRequest(nil, "/"))
	wantStatus(t, rec, http.StatusFound)
	if got := rec.Header().Get("Location"); !strings.HasPrefix(got, "https://sso.test/login?return=") {
		t.Fatalf("Location = %q", got)
	}
}

func TestLoginModeDirectSkipsPage(t *testing.T) {
	s := newTestRouter(newFakeControl(t, aliceRecord(), http.StatusOK))
	s.loginMode = "direct"
	rec := run(s.handleRoute, browserRequest(nil, "/"))
	wantStatus(t, rec, http.StatusFound)
	if got := rec.Header().Get("Location"); !strings.HasPrefix(got, "https://sso.test/login") {
		t.Fatalf("Location = %q", got)
	}
}

func TestLoginPage(t *testing.T) {
	s := newTestRouter(newFakeControl(t, aliceRecord(), http.StatusOK))
	rec := run(s.handleLogin, httptest.NewRequest(http.MethodGet, "/_dsh/login?next=%2Ffoo", nil))
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "使用企业账号登录")
	wantContains(t, rec, "/_dsh/start?next=%2Ffoo")
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "frame-ancestors 'none'") ||
		strings.Contains(got, "unsafe-inline") {
		t.Fatalf("CSP = %q", got)
	}
}

func TestLoginPageShowsNotice(t *testing.T) {
	s := newTestRouter(newFakeControl(t, aliceRecord(), http.StatusOK))
	rec := run(s.handleLogin, httptest.NewRequest(http.MethodGet, "/_dsh/login?e=expired", nil))
	wantStatus(t, rec, http.StatusOK)
	wantContains(t, rec, "会话已过期")
}

func TestLoginPageWithSessionRedirects(t *testing.T) {
	s := newTestRouter(newFakeControl(t, aliceRecord(), http.StatusOK))
	req := httptest.NewRequest(http.MethodGet, "/_dsh/login?next=%2Ffoo", nil)
	req.AddCookie(s.newSessionCookie("alice@corp", time.Now().Add(time.Hour)))
	rec := run(s.handleLogin, req)
	wantStatus(t, rec, http.StatusFound)
	if got := rec.Header().Get("Location"); got != "/foo" {
		t.Fatalf("Location = %q", got)
	}
}

func TestAuthStartBuildsCallback(t *testing.T) {
	s := newTestRouter(newFakeControl(t, aliceRecord(), http.StatusOK))
	req := httptest.NewRequest(http.MethodGet, "/_dsh/start?next=%2Ffoo", nil)
	req.Host = "alice.dsh.corp"
	rec := run(s.handleAuthStart, req)
	wantStatus(t, rec, http.StatusFound)

	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	ret := loc.Query().Get("return")
	if ret != "http://alice.dsh.corp/_auth/callback" {
		t.Fatalf("return = %q", ret)
	}
	var next *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == nextCookie {
			next = c
		}
	}
	if next == nil || next.Value != url.QueryEscape("/foo") {
		t.Fatalf("next cookie = %+v", next)
	}
}

func TestAuthStartSanitizesNext(t *testing.T) {
	s := newTestRouter(newFakeControl(t, aliceRecord(), http.StatusOK))
	req := httptest.NewRequest(http.MethodGet, "/_dsh/start?next=//evil.example", nil)
	req.Host = "alice.dsh.corp"
	rec := run(s.handleAuthStart, req)
	wantStatus(t, rec, http.StatusFound)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if strings.Contains(loc.Query().Get("return"), "evil") {
		t.Fatalf("return leaked an open redirect: %q", loc.Query().Get("return"))
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == nextCookie && strings.Contains(c.Value, "evil") {
			t.Fatalf("next cookie leaked an open redirect: %q", c.Value)
		}
	}
}

func ticketFor(s *Server, subject string, exp time.Time) string {
	e := strconv.FormatInt(exp.Unix(), 10)
	return subject + "|" + e + "|" + s.mac("ticket|"+subject+"|"+e)
}

func TestCallbackSetsSessionAndHonorsNext(t *testing.T) {
	s := newTestRouter(newFakeControl(t, aliceRecord(), http.StatusOK))
	tk := ticketFor(s, "alice@corp", time.Now().Add(time.Minute))
	req := httptest.NewRequest(http.MethodGet, "/_auth/callback?ticket="+url.QueryEscape(tk), nil)
	req.AddCookie(&http.Cookie{Name: nextCookie, Value: url.QueryEscape("/foo")})
	rec := run(s.handleCallback, req)
	wantStatus(t, rec, http.StatusFound)
	if got := rec.Header().Get("Location"); got != "/foo" {
		t.Fatalf("Location = %q", got)
	}
	var found bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == "dsh_sess" && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("dsh_sess cookie not set")
	}
}

func TestCallbackRejectsBadTicket(t *testing.T) {
	s := newTestRouter(newFakeControl(t, aliceRecord(), http.StatusOK))
	req := httptest.NewRequest(http.MethodGet, "/_auth/callback?ticket=alice%40corp%7C1%7Cdeadbeef", nil)
	wantStatus(t, run(s.handleCallback, req), http.StatusForbidden)
}

func TestCallbackRejectsOpenRedirect(t *testing.T) {
	s := newTestRouter(newFakeControl(t, aliceRecord(), http.StatusOK))
	tk := ticketFor(s, "alice@corp", time.Now().Add(time.Minute))
	req := httptest.NewRequest(http.MethodGet, "/_auth/callback?ticket="+url.QueryEscape(tk), nil)
	req.AddCookie(&http.Cookie{Name: nextCookie, Value: url.QueryEscape("https://evil.example")})
	rec := run(s.handleCallback, req)
	wantStatus(t, rec, http.StatusFound)
	if got := rec.Header().Get("Location"); got != "/" {
		t.Fatalf("Location = %q", got)
	}
}

func TestStartingPageForSleepingUser(t *testing.T) {
	rec := aliceRecord()
	rec.DesiredState = "sleeping"
	ctrl := newFakeControl(t, rec, http.StatusOK)
	s := newTestRouter(ctrl)

	out := run(s.handleRoute, browserRequest(s, "/"))
	wantStatus(t, out, http.StatusOK)
	wantContains(t, out, "正在为你启动工作区")
	if got := out.Header().Get("X-DSH-State"); got != "starting" {
		t.Fatalf("X-DSH-State = %q", got)
	}
	if got := out.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if calls := ctrl.desiredCalls(); len(calls) != 1 || calls[0] != "alice@corp:running" {
		t.Fatalf("wake calls = %v", calls)
	}
}

func TestStartingPageWakeIsThrottled(t *testing.T) {
	rec := aliceRecord()
	rec.DesiredState = "sleeping"
	ctrl := newFakeControl(t, rec, http.StatusOK)
	s := newTestRouter(ctrl)

	run(s.handleRoute, browserRequest(s, "/"))
	run(s.handleRoute, browserRequest(s, "/"))
	run(s.handleStatus, browserRequest(s, "/_dsh/status"))

	if calls := ctrl.desiredCalls(); len(calls) != 1 {
		t.Fatalf("wake should be throttled to one call, got %v", calls)
	}
}

func TestStartingPageCLIGets503(t *testing.T) {
	rec := aliceRecord()
	rec.DesiredState = "sleeping"
	s := newTestRouter(newFakeControl(t, rec, http.StatusOK))

	out := run(s.handleRoute, cliRequest(s, "/"))
	wantStatus(t, out, http.StatusServiceUnavailable)
	if got := out.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After = %q", got)
	}
	wantContains(t, out, "starting")
}

func TestRunningWithoutPodIPIsStarting(t *testing.T) {
	ctrl := newFakeControl(t, aliceRecord(), http.StatusOK) // running, podIP 为空
	s := newTestRouter(ctrl)

	page := run(s.handleRoute, browserRequest(s, "/"))
	wantStatus(t, page, http.StatusOK)
	wantContains(t, page, "正在为你启动工作区")

	api := run(s.handleRoute, cliRequest(s, "/"))
	wantStatus(t, api, http.StatusServiceUnavailable)

	if calls := ctrl.desiredCalls(); len(calls) != 0 {
		t.Fatalf("running user should not be woken again: %v", calls)
	}
}

func TestNotProvisioned(t *testing.T) {
	s := newTestRouter(newFakeControl(t, Record{}, http.StatusNotFound))

	page := run(s.handleRoute, browserRequest(s, "/"))
	wantStatus(t, page, http.StatusOK)
	wantContains(t, page, "工作区尚未开通")

	api := run(s.handleRoute, cliRequest(s, "/"))
	wantStatus(t, api, http.StatusServiceUnavailable)
	if got := api.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q", got)
	}

	st := run(s.handleStatus, browserRequest(s, "/_dsh/status"))
	wantStatus(t, st, http.StatusOK)
	var body statusResponse
	if err := json.Unmarshal(st.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.State != "not_provisioned" {
		t.Fatalf("state = %q", body.State)
	}
}

func TestArchivedUserIsNotWoken(t *testing.T) {
	rec := aliceRecord()
	rec.DesiredState = "archived"
	ctrl := newFakeControl(t, rec, http.StatusOK)
	s := newTestRouter(ctrl)

	page := run(s.handleRoute, browserRequest(s, "/"))
	wantStatus(t, page, http.StatusOK)
	wantContains(t, page, "工作区已归档")
	if calls := ctrl.desiredCalls(); len(calls) != 0 {
		t.Fatalf("archived user must not be woken: %v", calls)
	}
}

func TestHostMismatchWinsOverWaitingPage(t *testing.T) {
	rec := aliceRecord()
	rec.PublicHost = "bob.dsh.corp"
	rec.DesiredState = "sleeping"
	s := newTestRouter(newFakeControl(t, rec, http.StatusOK))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "alice.dsh.corp"
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.AddCookie(s.newSessionCookie("alice@corp", time.Now().Add(time.Hour)))

	wantStatus(t, run(s.handleRoute, req), http.StatusForbidden)
}

func TestStatusStates(t *testing.T) {
	cases := []struct {
		name     string
		rec      Record
		code     int
		wantCode int
		want     string
	}{
		{"sleeping", Record{DesiredState: "sleeping", PublicHost: "alice.dsh.corp", Subject: "alice@corp"}, http.StatusOK, 200, "starting"},
		{"running no pod", Record{DesiredState: "running", PublicHost: "alice.dsh.corp", Subject: "alice@corp"}, http.StatusOK, 200, "starting"},
		{"ready", Record{DesiredState: "running", PodIP: "10.0.0.7", PublicHost: "alice.dsh.corp", Subject: "alice@corp"}, http.StatusOK, 200, "ready"},
		{"archived", Record{DesiredState: "archived", PublicHost: "alice.dsh.corp", Subject: "alice@corp"}, http.StatusOK, 200, "archived"},
		{"missing", Record{}, http.StatusNotFound, 200, "not_provisioned"},
		{"control down", Record{}, http.StatusInternalServerError, 200, "error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestRouter(newFakeControl(t, c.rec, c.code))
			out := run(s.handleStatus, browserRequest(s, "/_dsh/status"))
			wantStatus(t, out, c.wantCode)
			var body statusResponse
			if err := json.Unmarshal(out.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.State != c.want {
				t.Fatalf("state = %q, want %q", body.State, c.want)
			}
		})
	}
}

func TestStatusWithoutSession(t *testing.T) {
	s := newTestRouter(newFakeControl(t, aliceRecord(), http.StatusOK))
	out := run(s.handleStatus, browserRequest(nil, "/_dsh/status"))
	wantStatus(t, out, http.StatusUnauthorized)
	var body statusResponse
	if err := json.Unmarshal(out.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.State != "expired" {
		t.Fatalf("state = %q", body.State)
	}
}

func TestStatusHostMismatch(t *testing.T) {
	rec := aliceRecord()
	rec.PublicHost = "bob.dsh.corp"
	s := newTestRouter(newFakeControl(t, rec, http.StatusOK))
	out := run(s.handleStatus, browserRequest(s, "/_dsh/status"))
	wantStatus(t, out, http.StatusForbidden)
	var body statusResponse
	if err := json.Unmarshal(out.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.State != "forbidden" {
		t.Fatalf("state = %q", body.State)
	}
}

func TestLogout(t *testing.T) {
	s := newTestRouter(newFakeControl(t, aliceRecord(), http.StatusOK))
	req := httptest.NewRequest(http.MethodPost, "/_dsh/logout", nil)
	req.Host = "alice.dsh.corp"
	req.Header.Set("Origin", "http://alice.dsh.corp")
	req.AddCookie(s.newSessionCookie("alice@corp", time.Now().Add(time.Hour)))

	out := run(s.handleLogout, req)
	wantStatus(t, out, http.StatusSeeOther)
	if got := out.Header().Get("Location"); got != "/_dsh/login?e=logout" {
		t.Fatalf("Location = %q", got)
	}
	cookies := out.Result().Cookies()
	if len(cookies) == 0 || cookies[0].MaxAge >= 0 {
		t.Fatalf("session cookie was not cleared: %+v", cookies)
	}
}

func TestLogoutRejectsCrossOrigin(t *testing.T) {
	s := newTestRouter(newFakeControl(t, aliceRecord(), http.StatusOK))
	req := httptest.NewRequest(http.MethodPost, "/_dsh/logout", nil)
	req.Host = "alice.dsh.corp"
	req.Header.Set("Origin", "https://evil.example")
	wantStatus(t, run(s.handleLogout, req), http.StatusForbidden)
}

func TestRoutesServeAssets(t *testing.T) {
	s := newTestRouter(newFakeControl(t, aliceRecord(), http.StatusOK))
	h := s.routes()

	css := httptest.NewRecorder()
	h.ServeHTTP(css, httptest.NewRequest(http.MethodGet, "/_dsh/assets/base.css", nil))
	wantStatus(t, css, http.StatusOK)
	if ct := css.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("css content-type = %q", ct)
	}
	wantContains(t, css, "--accent")

	js := httptest.NewRecorder()
	h.ServeHTTP(js, httptest.NewRequest(http.MethodGet, "/_dsh/assets/starting.js", nil))
	wantStatus(t, js, http.StatusOK)
	wantContains(t, js, `dataset.next`)

	health := httptest.NewRecorder()
	h.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	wantStatus(t, health, http.StatusOK)
	wantContains(t, health, "ok")
}

func TestAssetsDoNotExposeTemplatesOrListing(t *testing.T) {
	s := newTestRouter(newFakeControl(t, aliceRecord(), http.StatusOK))
	h := s.routes()
	for _, path := range []string{"/_dsh/assets/login.html", "/_dsh/assets/", "/_dsh/assets/missing.css"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404", path, rec.Code)
		}
	}
}

func TestPagesUseExternalAssets(t *testing.T) {
	s := newTestRouter(newFakeControl(t, aliceRecord(), http.StatusOK))

	login := run(s.handleLogin, httptest.NewRequest(http.MethodGet, "/_dsh/login", nil))
	wantStatus(t, login, http.StatusOK)
	wantContains(t, login, `/_dsh/assets/base.css`)
	if csp := login.Header().Get("Content-Security-Policy"); strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("CSP still allows inline assets: %q", csp)
	}

	page := run(s.handleRoute, browserRequest(s, "/"))
	wantStatus(t, page, http.StatusOK)
	wantContains(t, page, `/_dsh/assets/starting.js`)
	wantContains(t, page, `data-state="starting"`)
	wantContains(t, page, `data-next="/"`)
	if strings.Contains(page.Body.String(), "<script>") {
		t.Fatal("waiting page still contains an inline script")
	}
}

func (f *fakeControl) setUsers(list []Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users = list
}

func (f *fakeControl) provisioned() (map[string]any, string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.provisionIn, f.provisionBy, f.lookupAuth
}
