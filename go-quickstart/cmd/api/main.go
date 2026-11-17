// 这是程序的入口。
//
// 和 Java 的 main 类 / Node 的 index.js 不同，Go 的 main 只做"装配"：
// 读配置 → 建依赖 → 装路由 → 启服务 → 等信号 → 优雅退出。
// 业务逻辑一行都不该写在这里。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/goquickstart/internal/config"
	"example.com/goquickstart/internal/httpapi"
	"example.com/goquickstart/internal/store"
	"example.com/goquickstart/internal/user"
)

func main() {
	// log/slog 是 Go 1.21+ 的结构化日志标准库，直接输出 JSON。
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger) // 让第三方库里的 slog.Info 也走同一个 handler

	cfg := config.Load()

	// 依赖装配：store 是接口，将来换成 Postgres 只需改这一行。
	st := store.NewMemory()
	seed(context.Background(), st, logger)

	// 生产级 http.Server 必须显式设置超时，否则慢连接会拖垮进程。
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpapi.NewRouter(st, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// signal.NotifyContext：把 Ctrl-C / SIGTERM 变成一个 ctx.Done() 信号，
	// 等价于 JS 的 process.on('SIGINT')，但能顺着 ctx 传到每一层。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 监听放到 goroutine 里，主 goroutine 负责等退出信号。
	errCh := make(chan error, 1) // 缓冲 1：保证 goroutine 不会因为没人接收而泄漏
	go func() {
		logger.Info("http server started", "addr", cfg.Addr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "err", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		logger.Info("signal received, shutting down gracefully")
	}

	// 给在途请求最多 10 秒收尾
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "err", err)
	}
	logger.Info("bye")
}

// seed 塞两条初始数据，方便 curl 直接有东西看。
func seed(ctx context.Context, st store.Store, logger *slog.Logger) {
	seeds := []user.CreateInput{
		{Name: "Alice", Email: "alice@example.com"},
		{Name: "Bob", Email: "bob@example.com"},
	}
	for _, in := range seeds {
		if _, err := st.Create(ctx, in); err != nil {
			logger.Warn("seed failed", "email", in.Email, "err", err)
		}
	}
}
