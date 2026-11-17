package main

// 平台自己的两个页面：登录页、启动等待页，以及等待页轮询的状态接口。
//
// 为什么放在 router：
//   - 页面必须与 dsh_sess 同源，只有 router 知道 subject；
//   - 只有 router 看得到「记录存在、但 pod 还没注册」这个中间态；
//   - 页面模板与静态资源随二进制 embed，不依赖集群 egress（NetworkPolicy 只放行白名单）；
//     改 HTML/CSS/JS 不需要动 Go 代码。
//
// 语义约定：
//   - 浏览器导航（Sec-Fetch-Mode: navigate，或 Accept 含 text/html）永远看到页面；
//   - 其余客户端（curl、XHR、WebSocket）仍然拿到 503 + Retry-After，接口语义不变；
//   - 等待页返 200 而不是 503：企业代理常把 5xx 的 body 换成自己的错误页，
//     真正状态放在 X-DSH-State 头里，供监控与日志使用。

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
)

// nextCookie 保存「登录后回哪」；5 分钟有效，只作用于 /_auth。
const nextCookie = "dsh_next"

// userState 是「这个用户现在能不能进」的唯一分类，页面与状态接口共用。
type userState string

const (
	stateReady          userState = "ready"
	stateStarting       userState = "starting"
	stateNotProvisioned userState = "not_provisioned"
	stateArchived       userState = "archived"
	stateError          userState = "error"
	stateExpired        userState = "expired"
	stateForbidden      userState = "forbidden"
)

// stateOf 把控制面的一条记录折成页面能用的状态。
// 注意 podIP 为空、desiredState 已是 running 的窗口：必须算 starting，
// 否则等待页会拿空地址去做 token 交换，用户看到 502。
func stateOf(rec Record) userState {
	switch rec.DesiredState {
	case "archived":
		return stateArchived
	case "sleeping":
		return stateStarting
	}
	if rec.PodIP == "" {
		return stateStarting
	}
	return stateReady
}

// wantsHTML 判断这是不是一次「导航」。
// 浏览器顶层导航会带 Sec-Fetch-Mode: navigate；带了这个头就不再猜 Accept。
func wantsHTML(r *http.Request) bool {
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// safeNext 只接受同站相对路径，挡开放重定向。非法值一律退回首页。
func safeNext(raw string) string {
	if raw == "" {
		return "/"
	}
	if len(raw) > 512 || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/"
	}
	if strings.ContainsAny(raw, "\\\r\n\t") {
		return "/"
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" {
		return "/"
	}
	return raw
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// renderHTML 按模板名渲染。先写 buffer 再落盘，避免模板出错时留下半截 200。
// 页面只引用 /_dsh/assets/ 下的静态资源，所以 CSP 可以收紧到 'self'。
func (s *Server) renderHTML(ctx context.Context, w http.ResponseWriter, status int, name string, data any) {
	t := templates.Lookup(name)
	if t == nil {
		logFor(ctx).Error("render page: template not found", "template", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		logFor(ctx).Error("render page failed", "template", name, "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy",
		"default-src 'none'; style-src 'self'; script-src 'self'; "+
			"connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// serveStarting 是「目标没就绪」的唯一出口：页面或 503，二选一。
func (s *Server) serveStarting(w http.ResponseWriter, r *http.Request, st userState, next string) {
	w.Header().Set("X-DSH-State", string(st))
	w.Header().Set("Vary", "Accept, Sec-Fetch-Mode")
	if !wantsHTML(r) {
		if st == stateNotProvisioned {
			w.Header().Set("Retry-After", "30")
		} else {
			w.Header().Set("Retry-After", "3")
		}
		http.Error(w, string(st), http.StatusServiceUnavailable)
		return
	}
	title, detail := startingCopy(st)
	s.renderHTML(r.Context(), w, http.StatusOK, "starting.html", startingPageData{
		Brand:  s.brand,
		Title:  title,
		Detail: detail,
		State:  string(st),
		Next:   safeNext(next),
	})
}

func startingCopy(st userState) (title, detail string) {
	switch st {
	case stateNotProvisioned:
		return "工作区尚未开通", "你的账号还没有工作区，请联系 IT 管理员开通。页面会自动重试。"
	case stateArchived:
		return "工作区已归档", "长期未使用已归档，请联系 IT 管理员恢复。"
	case stateError:
		return "控制面暂时不可用", "正在自动重试，通常很快恢复。"
	default:
		return "正在为你启动工作区", "首次打开或刚唤醒时需要几秒到十几秒，完成后会自动进入。"
	}
}

func loginNotice(code string) string {
	switch code {
	case "expired":
		return "会话已过期，请重新登录。"
	case "logout":
		return "你已退出登录。"
	case "denied":
		return "当前账号没有访问权限，请联系 IT 管理员。"
	}
	return ""
}

// beginLogin：没有会话时的入口。浏览器导航给登录页（一次点击），
// 其他客户端保持原来的 302 直达 SSO，避免破坏脚本与监控。
func (s *Server) beginLogin(w http.ResponseWriter, r *http.Request) {
	if s.preferLoginPage(r) {
		next := safeNext(r.URL.RequestURI())
		http.Redirect(w, r, "/_dsh/login?next="+url.QueryEscape(next), http.StatusFound)
		return
	}
	s.redirectToSSO(w, r, "/")
}

func (s *Server) preferLoginPage(r *http.Request) bool {
	if s.loginMode == "direct" {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return wantsHTML(r)
}

// redirectToSSO 保持回调地址干净（就是文档里的 /_auth/callback），
// 原始路径放进 5 分钟有效的 dsh_next cookie —— 有些企业 SSO 的 return 白名单
// 只匹配固定路径，带查询串会被拒。
func (s *Server) redirectToSSO(w http.ResponseWriter, r *http.Request, next string) {
	if n := safeNext(next); n != "/" {
		http.SetCookie(w, &http.Cookie{
			Name:     nextCookie,
			Value:    url.QueryEscape(n),
			Path:     "/_auth",
			MaxAge:   300,
			HttpOnly: true,
			Secure:   s.cookieSecure,
			SameSite: http.SameSiteLaxMode,
		})
	}
	cb := s.publicScheme + "://" + r.Host + "/_auth/callback"
	u := s.ssoURL + "?return=" + url.QueryEscape(cb)
	// 本地假 IdP：按子域决定身份（usera.dsh.corp -> usera@corp），
	// 这样一台机器能分别验多个用户；生产走真实 IdP，这段不会执行。
	if subj := s.devSubjectForHost(r.Host); subj != "" {
		u += "&subject=" + url.QueryEscape(subj)
	}
	http.Redirect(w, r, u, http.StatusFound)
}

// takeNext 读回登录前想去的位置，读完立即清掉。
func (s *Server) takeNext(w http.ResponseWriter, r *http.Request) string {
	next := "/"
	if c, err := r.Cookie(nextCookie); err == nil {
		if v, err := url.QueryUnescape(c.Value); err == nil {
			next = safeNext(v)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     nextCookie,
		Value:    "",
		Path:     "/_auth",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	return next
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	next := safeNext(r.URL.Query().Get("next"))
	if next == "/_dsh/login" {
		next = "/"
	}
	// 已有有效会话就不必再走一遍登录页。
	if _, ok := s.session(r); ok {
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	s.renderHTML(r.Context(), w, http.StatusOK, "login.html", loginPageData{
		Brand:    s.brand,
		StartURL: "/_dsh/start?next=" + url.QueryEscape(next),
		Notice:   loginNotice(r.URL.Query().Get("e")),
	})
}

func (s *Server) handleAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.redirectToSSO(w, r, r.URL.Query().Get("next"))
}

// handleLogout 清掉 router 的会话 cookie。它不影响 DSH 自己的 cookie，
// 那份只在 router 与 pod 之间传递，用户侧看不到。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !sameAuthority(u.Host, r.Host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "dsh_sess",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/_dsh/login?e=logout", http.StatusSeeOther)
}

type statusResponse struct {
	State        string `json:"state"`
	RetryAfterMs int    `json:"retryAfterMs,omitempty"`
}

// handleStatus 供等待页轮询。subject 只从 cookie 来，不接受查询参数，
// 所以拿不到别人的状态；Host 仍然要跟记录里的 publicHost 对得上。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	subject, ok := s.session(r)
	if !ok {
		s.writeJSON(w, http.StatusUnauthorized, statusResponse{State: string(stateExpired)})
		return
	}
	rec, code := s.lookup(r.Context(), subject)
	switch code {
	case http.StatusNotFound:
		s.writeJSON(w, http.StatusOK, statusResponse{State: string(stateNotProvisioned), RetryAfterMs: 8000})
		return
	case http.StatusOK:
		// 继续判断
	default:
		s.writeJSON(w, http.StatusOK, statusResponse{State: string(stateError), RetryAfterMs: 4000})
		return
	}
	if !sameAuthority(r.Host, rec.PublicHost) {
		s.writeJSON(w, http.StatusForbidden, statusResponse{State: string(stateForbidden)})
		return
	}

	state := stateOf(rec)
	if state == stateStarting && rec.DesiredState == "sleeping" {
		s.wakeThrottled(r.Context(), subject)
	}
	retry := 0
	if state == stateStarting {
		retry = 2000
	}
	s.writeJSON(w, http.StatusOK, statusResponse{State: string(state), RetryAfterMs: retry})
}

type loginPageData struct {
	Brand    string
	StartURL string
	Notice   string
}

type startingPageData struct {
	Brand  string
	Title  string
	Detail string
	State  string
	Next   string
}

// 前端资源与二进制一起发布：模板在 web/templates，样式与脚本在 web/assets。
// 改页面不需要碰 Go；embed 在编译期生效，所以改完要重新构建镜像。
//
//go:embed web/templates/*.html web/assets/*
var webFS embed.FS

var (
	templates   = template.Must(template.ParseFS(webFS, "web/templates/*.html"))
	assetsFS    = mustSubFS(webFS, "web/assets")
	assetServer = http.StripPrefix("/_dsh/assets/", http.FileServer(http.FS(assetsFS)))
)

func mustSubFS(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// handleAssets 只暴露 web/assets 下的静态文件；模板目录不在这个子文件系统里，
// 所以 /_dsh/assets/login.html 自然 404。
func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_dsh/assets/" {
		http.NotFound(w, r)
		return
	}
	// 资源没做内容哈希，宁可每次回源也不要让新模板配旧 CSS/JS。
	w.Header().Set("Cache-Control", "no-cache")
	assetServer.ServeHTTP(w, r)
}
