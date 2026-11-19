package main

// 管理员页面：新增用户、查看状态、休眠/唤醒。
//
// 分工：
//   - router 负责认证（dsh_sess + ADMIN_SUBJECTS 白名单）、页面、CSRF 与转发；
//   - control 负责真正的开通（K8s 资源 + 数据库登记），router 不持有 K8s 权限。
//
// 安全边界：
//   - 管理台默认关闭：ADMIN_TOKEN 或 ADMIN_SUBJECTS 为空时一律 404；
//   - 所有写操作校验 Origin 与 application/json，降低 CSRF 面；
//   - launch token 不进入管理台响应（control 侧已脱敏，这里再兜一层）。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const adminPath = "/_dsh/admin"

func parseSubjects(raw string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		if s := strings.ToLower(strings.TrimSpace(part)); s != "" {
			out[s] = true
		}
	}
	return out
}

func (s *Server) adminEnabled() bool {
	return s.adminToken != "" && len(s.adminSubjects) > 0
}

func (s *Server) isAdmin(subject string) bool {
	return s.adminSubjects[strings.ToLower(subject)]
}

// requireAdmin 处理页面与 API 的差异：导航去登录/403 页面，接口返回 JSON 状态码。
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request, api bool) (string, bool) {
	if !s.adminEnabled() {
		http.NotFound(w, r)
		return "", false
	}
	subject, ok := s.session(r)
	if !ok {
		if api {
			s.writeJSON(w, http.StatusUnauthorized, statusResponse{State: string(stateExpired)})
			return "", false
		}
		next := url.QueryEscape(adminPath)
		http.Redirect(w, r, "/_dsh/login?next="+next, http.StatusFound)
		return "", false
	}
	if !s.isAdmin(subject) {
		if api {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "not an admin"})
		} else {
			http.Error(w, "forbidden: not an admin", http.StatusForbidden)
		}
		return "", false
	}
	return subject, true
}

type adminPageData struct {
	Brand      string
	Admin      string
	BaseDomain string
}

func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	subject, ok := s.requireAdmin(w, r, false)
	if !ok {
		return
	}
	s.renderHTML(r.Context(), w, http.StatusOK, "admin.html", adminPageData{
		Brand:      s.brand,
		Admin:      subject,
		BaseDomain: s.baseDomain,
	})
}

func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	subject, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		code, body, err := s.controlRequest(r.Context(), http.MethodGet, "/internal/users", nil, subject)
		if err != nil {
			s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		s.relayJSON(w, code, stripLaunchTokens(body))
	case http.MethodPost:
		if !sameOrigin(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request rejected"})
			return
		}
		if !isJSON(r) {
			s.writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "content-type must be application/json"})
			return
		}
		var in struct {
			Subject    string `json:"subject"`
			Tier       string `json:"tier"`
			ModelToken string `json:"modelToken"`
		}
		if !decodeAdminJSON(w, r, &in) {
			return
		}
		code, body, err := s.controlRequest(r.Context(), http.MethodPost, "/internal/provision", in, subject)
		if err != nil {
			s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		s.relayJSON(w, code, body)
	case http.MethodDelete:
		if !sameOrigin(r) {
			s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request rejected"})
			return
		}
		sub := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("subject")))
		if sub == "" {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "subject required"})
			return
		}
		// 默认软删除（数据保留、立即不可见）；keepData=1 连 PVC 一起保留
		path := "/internal/users?subject=" + url.QueryEscape(sub)
		// 默认保留数据卷；只有显式 keepData=0 才让 control 删 PVC
		if r.URL.Query().Get("keepData") == "0" {
			path += "&keepData=0"
		}
		code, body, err := s.controlRequest(r.Context(), http.MethodDelete, path, nil, subject)
		if err != nil {
			s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		s.relayJSON(w, code, body)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) handleAdminDesired(w http.ResponseWriter, r *http.Request) {
	subject, ok := s.requireAdmin(w, r, true)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if !sameOrigin(r) || !isJSON(r) {
		s.writeJSON(w, http.StatusForbidden, map[string]string{"error": "bad request origin or content-type"})
		return
	}
	var in struct {
		Subject      string `json:"subject"`
		DesiredState string `json:"desiredState"`
		ClearPod     bool   `json:"clearPod"`
	}
	if !decodeAdminJSON(w, r, &in) {
		return
	}
	code, body, err := s.controlRequest(r.Context(), http.MethodPost, "/internal/desired", in, subject)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.relayJSON(w, code, body)
}

// controlRequest 用管理员令牌调用控制面；actor 放进头里，便于审计日志。
func (s *Server) controlRequest(ctx context.Context, method, path string, payload any, actor string) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 70*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, s.controlURL+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.adminToken)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if actor != "" {
		req.Header.Set("X-DSH-Actor", actor)
	}
	// 管理台发起的一次操作，其 id 会出现在 control 的阶段日志里
	setRequestID(req, ctx)
	resp, err := (&http.Client{Timeout: 70 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, out, nil
}

// stripLaunchTokens 在转发层再脱一次敏：launch token 等价于 pod 的完整工具权限，
// 管理台只需要路由与活动状态。控制面已经清空，这里是防御性兜底。
func stripLaunchTokens(body []byte) []byte {
	var list []map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		return body
	}
	for _, item := range list {
		delete(item, "launchToken")
		delete(item, "launch_token")
	}
	out, err := json.Marshal(list)
	if err != nil {
		return body
	}
	return out
}

func (s *Server) relayJSON(w http.ResponseWriter, code int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func decodeAdminJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	return true
}

func isJSON(r *http.Request) bool {
	return strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json")
}

// sameOrigin 只拦浏览器跨站写请求；没有 Origin 的调用（curl）由管理员令牌与会话兜底。
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && sameAuthority(u.Host, r.Host)
}
