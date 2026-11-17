// Package user 是领域层：只描述"用户是什么"，不关心存在哪里、怎么传输。
//
// 和 Java 的 Entity 不同，这里的结构体没有任何注解、没有基类，
// 就是一个纯数据 + 行为的结构体。
package user

import (
	"strings"
	"time"
)

// User 是领域模型。
// 反引号里的 `json:"..."` 是 struct tag：给 encoding/json 的元信息，
// 相当于 Java 的 @JsonProperty，但不参与运行时反射之外的任何逻辑。
type User struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateInput 是"创建用户"的入参。
// Go 里习惯给每个操作单独定义一个输入结构体，而不是复用 User，
// 因为 User.ID / CreatedAt 是服务端生成的，不该由客户端传。
type CreateInput struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// UpdateInput 是部分更新（PATCH）的入参。
// 字段用指针：nil 表示"没传这个字段"，非 nil 表示"传了，更新成这个值"。
// 这是 Go 里表达"可选字段"的标准做法（没有 undefined 可用）。
type UpdateInput struct {
	Name  *string `json:"name"`
	Email *string `json:"email"`
}

// ValidationError 是一个"错误类型"。
// 自定义错误 = 实现 error 接口（只要一个 Error() string 方法），
// 调用方用 errors.As 把它还原出来，从而把字段级错误返回给前端。
type ValidationError struct {
	Fields map[string]string
}

// Error 实现 error 接口。
func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for k, v := range e.Fields {
		parts = append(parts, k+": "+v)
	}
	return "validation failed: " + strings.Join(parts, ", ")
}

// Validate 是手写校验。Go 社区更偏手写或轻量库（go-playground/validator），
// 不像 Spring 那样靠注解。
func (in CreateInput) Validate() error {
	errs := map[string]string{}

	name := strings.TrimSpace(in.Name)
	if name == "" {
		errs["name"] = "不能为空"
	} else if len([]rune(name)) > 64 {
		errs["name"] = "最多 64 个字符"
	}

	email := strings.TrimSpace(in.Email)
	if email == "" {
		errs["email"] = "不能为空"
	} else if !strings.Contains(email, "@") {
		errs["email"] = "格式不正确"
	}

	if len(errs) > 0 {
		return &ValidationError{Fields: errs}
	}
	return nil
}

// Validate 校验部分更新：显式传了的字段才检查。
func (in UpdateInput) Validate() error {
	if in.Name == nil && in.Email == nil {
		return &ValidationError{Fields: map[string]string{"body": "至少提供 name 或 email 之一"}}
	}

	errs := map[string]string{}
	if in.Name != nil {
		if strings.TrimSpace(*in.Name) == "" {
			errs["name"] = "不能为空"
		} else if len([]rune(*in.Name)) > 64 {
			errs["name"] = "最多 64 个字符"
		}
	}
	if in.Email != nil {
		if strings.TrimSpace(*in.Email) == "" {
			errs["email"] = "不能为空"
		} else if !strings.Contains(*in.Email, "@") {
			errs["email"] = "格式不正确"
		}
	}

	if len(errs) > 0 {
		return &ValidationError{Fields: errs}
	}
	return nil
}
