package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"example.com/goquickstart/internal/store"
	"example.com/goquickstart/internal/user"
)

// handlers 是所有 HTTP handler 的宿主。
// 依赖通过结构体字段注入（而不是全局变量/singleton），测试时换个 store 就行。
type handlers struct {
	store  store.Store
	logger *slog.Logger
}

func (h *handlers) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handlers) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.store.List(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": users,
		"count": len(users),
	})
}

func (h *handlers) createUser(w http.ResponseWriter, r *http.Request) {
	var in user.CreateInput
	if err := decodeJSON(w, r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := in.Validate(); err != nil {
		h.fail(w, r, err)
		return
	}

	u, err := h.store.Create(r.Context(), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (h *handlers) getUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "id 必须是正整数")
		return
	}

	u, err := h.store.Get(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (h *handlers) updateUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "id 必须是正整数")
		return
	}

	var in user.UpdateInput
	if err := decodeJSON(w, r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := in.Validate(); err != nil {
		h.fail(w, r, err)
		return
	}

	u, err := h.store.Update(r.Context(), id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (h *handlers) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", "id 必须是正整数")
		return
	}

	if err := h.store.Delete(r.Context(), id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent) // 204 不带 body
}

// fail 是唯一的 "error -> HTTP 状态码" 翻译层。
//
// 用 errors.As / errors.Is 而不是比较字符串：即使错误被 fmt.Errorf("%w") 包装过，
// 也能正确识别出底层类型。这是 Go 错误处理的核心套路。
func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ve *user.ValidationError

	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusUnprocessableEntity, errorBody{
			Error:   "validation_failed",
			Message: ve.Error(),
			Fields:  ve.Fields,
		})
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "用户不存在")
	case errors.Is(err, store.ErrEmailTaken):
		writeError(w, http.StatusConflict, "email_taken", "email 已被占用")
	default:
		// 未知错误：记详细日志，对客户端只返回 500，不泄露内部细节
		h.logger.Error("unhandled error",
			"err", err,
			"method", r.Method,
			"path", r.URL.Path,
			"request_id", RequestIDFrom(r.Context()),
		)
		writeError(w, http.StatusInternalServerError, "internal_error", "服务器内部错误")
	}
}

// pathID 解析 /api/users/{id} 里的路径参数。
// r.PathValue 是 Go 1.22 新增的 API，省掉了手工正则。
func pathID(r *http.Request) (int64, error) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id: " + raw)
	}
	return id, nil
}
