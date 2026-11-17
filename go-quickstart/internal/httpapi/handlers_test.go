package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"example.com/goquickstart/internal/httpapi"
	"example.com/goquickstart/internal/store"
)

// newTestServer 用 httptest 起一个真实的 HTTP 服务（随机端口），
// 测完由 t.Cleanup 自动关闭。这样测试跑的是完整链路：路由 + 中间件 + handler。
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	// 测试里把日志丢进 io.Discard，避免污染 go test 输出
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := httptest.NewServer(httpapi.NewRouter(store.NewMemory(), logger))
	t.Cleanup(srv.Close)
	return srv
}

func doJSON(t *testing.T, method, url, body string) *http.Response {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("发送请求失败: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeBody(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
}

func TestHealth(t *testing.T) {
	srv := newTestServer(t)

	resp := doJSON(t, http.MethodGet, srv.URL+"/healthz", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// 一条完整链路：创建 -> 查询 -> 列表 -> 部分更新 -> 删除 -> 再查 404
func TestUserLifecycle(t *testing.T) {
	srv := newTestServer(t)

	// --- 创建 ---
	resp := doJSON(t, http.MethodPost, srv.URL+"/api/users",
		`{"name":"Carol","email":"carol@example.com"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("创建: status = %d, want 201", resp.StatusCode)
	}

	var created struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	decodeBody(t, resp, &created)
	if created.ID == 0 || created.Name != "Carol" {
		t.Fatalf("创建返回异常: %+v", created)
	}

	// --- 查询单个 ---
	resp = doJSON(t, http.MethodGet, fmt.Sprintf("%s/api/users/%d", srv.URL, created.ID), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查询: status = %d, want 200", resp.StatusCode)
	}

	// --- 列表 ---
	resp = doJSON(t, http.MethodGet, srv.URL+"/api/users", "")
	var list struct {
		Count int `json:"count"`
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	decodeBody(t, resp, &list)
	if list.Count != 1 || len(list.Items) != 1 {
		t.Fatalf("列表返回异常: %+v", list)
	}

	// --- 部分更新：只改 name ---
	resp = doJSON(t, http.MethodPatch, fmt.Sprintf("%s/api/users/%d", srv.URL, created.ID),
		`{"name":"Carol2"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("更新: status = %d, want 200", resp.StatusCode)
	}
	var updated struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	decodeBody(t, resp, &updated)
	if updated.Name != "Carol2" || updated.Email != "carol@example.com" {
		t.Fatalf("更新结果异常: %+v", updated)
	}

	// --- 删除 ---
	resp = doJSON(t, http.MethodDelete, fmt.Sprintf("%s/api/users/%d", srv.URL, created.ID), "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("删除: status = %d, want 204", resp.StatusCode)
	}

	// --- 再查应该 404 ---
	resp = doJSON(t, http.MethodGet, fmt.Sprintf("%s/api/users/%d", srv.URL, created.ID), "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("删除后查询: status = %d, want 404", resp.StatusCode)
	}
}

func TestCreateUserErrors(t *testing.T) {
	srv := newTestServer(t)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"非法 JSON", `{`, http.StatusBadRequest},
		{"未知字段", `{"name":"A","email":"a@b.com","nick":"x"}`, http.StatusBadRequest},
		{"多余内容", `{"name":"A","email":"a@b.com"}{"x":1}`, http.StatusBadRequest},
		{"缺少 name", `{"email":"a@b.com"}`, http.StatusUnprocessableEntity},
		{"邮箱格式错误", `{"name":"A","email":"nope"}`, http.StatusUnprocessableEntity},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := doJSON(t, http.MethodPost, srv.URL+"/api/users", c.body)
			if resp.StatusCode != c.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.want)
			}

			var body struct {
				Error string `json:"error"`
			}
			decodeBody(t, resp, &body)
			if body.Error == "" {
				t.Error("错误响应应包含 error 字段")
			}
		})
	}
}

func TestEmailConflict(t *testing.T) {
	srv := newTestServer(t)

	body := `{"name":"A","email":"dup@example.com"}`
	if resp := doJSON(t, http.MethodPost, srv.URL+"/api/users", body); resp.StatusCode != http.StatusCreated {
		t.Fatalf("首次创建: status = %d", resp.StatusCode)
	}
	if resp := doJSON(t, http.MethodPost, srv.URL+"/api/users", body); resp.StatusCode != http.StatusConflict {
		t.Fatalf("重复邮箱: status = %d, want 409", resp.StatusCode)
	}
}

func TestInvalidPathID(t *testing.T) {
	srv := newTestServer(t)

	for _, path := range []string{"/api/users/abc", "/api/users/0", "/api/users/-1"} {
		resp := doJSON(t, http.MethodGet, srv.URL+path, "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", path, resp.StatusCode)
		}
	}
}

// 标准库 ServeMux 会自动返回 405（路径匹配但方法不匹配）
func TestMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t)

	resp := doJSON(t, http.MethodPost, srv.URL+"/api/users/1", `{}`)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}
