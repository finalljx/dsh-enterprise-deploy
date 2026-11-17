package store_test

import (
	"context"
	"errors"
	"testing"

	"example.com/goquickstart/internal/store"
	"example.com/goquickstart/internal/user"
)

// 表驱动测试（table-driven test）是 Go 最主流的测试写法：
// 用例是一个切片，循环里用 t.Run 起子测试，失败时能精确定位到用例名。
func TestMemoryCreate(t *testing.T) {
	cases := []struct {
		name      string
		input     user.CreateInput
		wantName  string
		wantEmail string
	}{
		{
			name:      "去空格并小写邮箱",
			input:     user.CreateInput{Name: "  Alice  ", Email: "  ALICE@Example.COM "},
			wantName:  "Alice",
			wantEmail: "alice@example.com",
		},
		{
			name:      "普通输入",
			input:     user.CreateInput{Name: "Bob", Email: "bob@example.com"},
			wantName:  "Bob",
			wantEmail: "bob@example.com",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := store.NewMemory()

			got, err := st.Create(context.Background(), c.input)
			if err != nil {
				t.Fatalf("Create() 返回错误: %v", err)
			}
			if got.ID == 0 {
				t.Fatal("Create() 应该分配自增 ID")
			}
			if got.Name != c.wantName {
				t.Errorf("Name = %q, want %q", got.Name, c.wantName)
			}
			if got.Email != c.wantEmail {
				t.Errorf("Email = %q, want %q", got.Email, c.wantEmail)
			}
			if got.CreatedAt.IsZero() {
				t.Error("CreatedAt 不应为零值")
			}
		})
	}
}

func TestMemoryCRUD(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	created, err := st.Create(ctx, user.CreateInput{Name: "Alice", Email: "alice@example.com"})
	if err != nil {
		t.Fatalf("Create() 失败: %v", err)
	}

	// --- 查询 ---
	got, err := st.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get() 失败: %v", err)
	}
	if got.Name != "Alice" {
		t.Errorf("Get().Name = %q, want Alice", got.Name)
	}

	// --- 邮箱唯一 ---
	if _, err := st.Create(ctx, user.CreateInput{Name: "X", Email: "ALICE@example.com"}); !errors.Is(err, store.ErrEmailTaken) {
		t.Errorf("重复邮箱应返回 ErrEmailTaken, got %v", err)
	}

	// --- 部分更新：只改 name，email 不动 ---
	newName := "Alice Cooper"
	updated, err := st.Update(ctx, created.ID, user.UpdateInput{Name: &newName})
	if err != nil {
		t.Fatalf("Update() 失败: %v", err)
	}
	if updated.Name != newName || updated.Email != "alice@example.com" {
		t.Errorf("Update() = %+v", updated)
	}

	// --- 列表按 ID 排序 ---
	if _, err := st.Create(ctx, user.CreateInput{Name: "Bob", Email: "bob@example.com"}); err != nil {
		t.Fatalf("Create() 失败: %v", err)
	}
	list, err := st.List(ctx)
	if err != nil {
		t.Fatalf("List() 失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List() 长度 = %d, want 2", len(list))
	}
	if list[0].ID > list[1].ID {
		t.Error("List() 结果应带头部有序")
	}

	// --- 不存在的 ID ---
	if _, err := st.Get(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get(999) 应返回 ErrNotFound, got %v", err)
	}

	// --- 删除 ---
	if err := st.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete() 失败: %v", err)
	}
	if _, err := st.Get(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("删除后 Get() 应返回 ErrNotFound, got %v", err)
	}
	// 删掉后邮箱应该被释放
	if _, err := st.Create(ctx, user.CreateInput{Name: "Alice2", Email: "alice@example.com"}); err != nil {
		t.Errorf("删除后邮箱应可复用, got %v", err)
	}
}

// 验证"返回值是副本"：改返回值不能影响内部状态。
func TestMemoryReturnsCopy(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	created, _ := st.Create(ctx, user.CreateInput{Name: "Alice", Email: "a@example.com"})
	created.Name = "Hacked"

	got, err := st.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get() 失败: %v", err)
	}
	if got.Name != "Alice" {
		t.Errorf("外部修改污染了存储内部数据: Name = %q", got.Name)
	}
}

func TestUpdateEmailConflict(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()

	a, _ := st.Create(ctx, user.CreateInput{Name: "A", Email: "a@example.com"})
	_, _ = st.Create(ctx, user.CreateInput{Name: "B", Email: "b@example.com"})

	conflict := "b@example.com"
	if _, err := st.Update(ctx, a.ID, user.UpdateInput{Email: &conflict}); !errors.Is(err, store.ErrEmailTaken) {
		t.Errorf("改成别人的邮箱应返回 ErrEmailTaken, got %v", err)
	}
}
