// Package store 是持久层抽象。
//
// Go 的接口是"隐式实现"：Memory 结构体不需要写 implements，
// 只要方法签名对得上，就自动满足 Store。
// 这就是为什么 Go 里"面向接口编程"成本极低：接口由使用方定义。
package store

import (
	"context"
	"errors"

	"example.com/goquickstart/internal/user"
)

// 哨兵错误（sentinel error）：包对外暴露的错误值，
// 调用方用 errors.Is(err, store.ErrNotFound) 判断，而不是比字符串。
var (
	ErrNotFound   = errors.New("user not found")
	ErrEmailTaken = errors.New("email already taken")
)

// Store 是持久层契约。
//
// 注意每个方法都带 context.Context：Go 里所有可能阻塞/跨网络的调用
// 都应该接受 ctx，用来传递取消信号和超时。它相当于把 Java 的
// Thread.interrupt / JS 的 AbortSignal 变成显式参数。
type Store interface {
	Create(ctx context.Context, in user.CreateInput) (*user.User, error)
	Get(ctx context.Context, id int64) (*user.User, error)
	List(ctx context.Context) ([]*user.User, error)
	Update(ctx context.Context, id int64, in user.UpdateInput) (*user.User, error)
	Delete(ctx context.Context, id int64) error
}

// 编译期断言：确保 *Memory 实现了 Store。
// 这行代码不产生任何运行时开销，但能在改接口时立刻报错。
var _ Store = (*Memory)(nil)
