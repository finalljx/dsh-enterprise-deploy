package store

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"example.com/goquickstart/internal/user"
)

// Memory 是内存实现，方便入门和测试。
// 换成 PostgreSQL 时，只需要新写一个同样实现 Store 的结构体，上层一行都不用改。
type Memory struct {
	// sync.RWMutex：读多写少场景的标准锁。
	// 注意它不能拷贝（所以方法都用指针接收者），go vet 会检查这一点。
	mu      sync.RWMutex
	nextID  int64
	byID    map[int64]*user.User
	byEmail map[string]int64
}

// NewMemory 是构造函数。
// Go 没有 constructor，惯例是写一个返回 *T 的 NewXxx 函数。
func NewMemory() *Memory {
	return &Memory{
		byID:    make(map[int64]*user.User),
		byEmail: make(map[string]int64),
	}
}

func (m *Memory) Create(ctx context.Context, in user.CreateInput) (*user.User, error) {
	// Lock/Unlock 成对出现，用 defer 保证 panic 时也能解锁
	m.mu.Lock()
	defer m.mu.Unlock()

	email := normalizeEmail(in.Email)
	if _, exists := m.byEmail[email]; exists {
		return nil, ErrEmailTaken
	}

	m.nextID++
	u := &user.User{
		ID:        m.nextID,
		Name:      strings.TrimSpace(in.Name),
		Email:     email,
		CreatedAt: time.Now().UTC(),
	}
	m.byID[u.ID] = u
	m.byEmail[email] = u.ID

	return clone(u), nil
}

func (m *Memory) Get(ctx context.Context, id int64) (*user.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	u, ok := m.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(u), nil
}

func (m *Memory) List(ctx context.Context) ([]*user.User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]*user.User, 0, len(m.byID))
	for _, u := range m.byID {
		out = append(out, clone(u))
	}
	// map 遍历顺序是随机的，接口返回前要显式排序
	slices.SortFunc(out, func(a, b *user.User) int {
		return cmp.Compare(a.ID, b.ID)
	})
	return out, nil
}

func (m *Memory) Update(ctx context.Context, id int64, in user.UpdateInput) (*user.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	u, ok := m.byID[id]
	if !ok {
		return nil, ErrNotFound
	}

	if in.Email != nil {
		newEmail := normalizeEmail(*in.Email)
		if newEmail != u.Email {
			if _, taken := m.byEmail[newEmail]; taken {
				return nil, ErrEmailTaken
			}
			delete(m.byEmail, u.Email)
			m.byEmail[newEmail] = id
			u.Email = newEmail
		}
	}
	if in.Name != nil {
		u.Name = strings.TrimSpace(*in.Name)
	}

	return clone(u), nil
}

func (m *Memory) Delete(ctx context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	u, ok := m.byID[id]
	if !ok {
		return ErrNotFound
	}
	delete(m.byID, id)
	delete(m.byEmail, u.Email)
	return nil
}

// clone 返回一份副本，避免调用方改到存储内部的数据。
//
// Go 的 struct 赋值就是逐字段拷贝（值语义），
// 所以 `c := *u` 已经是一次完整拷贝；只有 map/slice/指针字段需要额外深拷贝。
func clone(u *user.User) *user.User {
	c := *u
	return &c
}

func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
