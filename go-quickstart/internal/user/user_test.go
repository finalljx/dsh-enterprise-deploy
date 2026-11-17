package user_test

import (
	"errors"
	"testing"

	"example.com/goquickstart/internal/user"
)

func TestCreateInputValidate(t *testing.T) {
	cases := []struct {
		name    string
		input   user.CreateInput
		wantErr bool
	}{
		{"合法输入", user.CreateInput{Name: "Alice", Email: "alice@example.com"}, false},
		{"空名字", user.CreateInput{Name: "   ", Email: "a@b.com"}, true},
		{"空邮箱", user.CreateInput{Name: "Alice", Email: ""}, true},
		{"邮箱缺 @", user.CreateInput{Name: "Alice", Email: "nope"}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.input.Validate()
			if (err != nil) != c.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, c.wantErr)
			}

			if err == nil {
				return
			}
			// 用 errors.As 取回具体错误类型，检查字段级信息
			var ve *user.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("错误类型应为 *user.ValidationError, got %T", err)
			}
			if len(ve.Fields) == 0 {
				t.Error("ValidationError.Fields 不应为空")
			}
		})
	}
}

func TestUpdateInputValidate(t *testing.T) {
	name := "New"
	empty := "   "

	t.Run("什么都不传", func(t *testing.T) {
		if err := (user.UpdateInput{}).Validate(); err == nil {
			t.Fatal("应返回错误")
		}
	})

	t.Run("只传 name", func(t *testing.T) {
		if err := (user.UpdateInput{Name: &name}).Validate(); err != nil {
			t.Fatalf("不应报错: %v", err)
		}
	})

	t.Run("name 为空串", func(t *testing.T) {
		if err := (user.UpdateInput{Name: &empty}).Validate(); err == nil {
			t.Fatal("应返回错误")
		}
	})
}
