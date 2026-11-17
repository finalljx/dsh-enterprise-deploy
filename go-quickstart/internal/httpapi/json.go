package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// errorBody 是统一的错误响应体。
type errorBody struct {
	Error   string            `json:"error"`
	Message string            `json:"message,omitempty"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// writeJSON 把任意值序列化并写回。
// 参数类型是 any（= interface{}），相当于 Java 的 Object、TS 的 unknown。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// 响应头已经发出去，改不了状态码了，只能记录
		// （生产代码这里应该注入 logger；为了保持示例简单，先忽略）
		return
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: code, Message: message})
}

// decodeJSON 是解码 body 的唯一入口，把三件事收口在一处：
//  1. 限制 body 大小（防 OOM / 慢速攻击）
//  2. 拒绝未知字段（拼错字段名会报错，而不是静默丢弃）
//  3. 拒绝多余内容（body 里只允许一个 JSON 对象）
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("解析 JSON 失败: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("请求体只能包含一个 JSON 对象")
	}
	return nil
}
