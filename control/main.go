// dsh-control: 用户注册表 + 活动状态 + launch token 存储（Postgres 实现）
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(v); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	return true
}

func main() {
	logger := setupLogging()

	adminToken := mustEnv("ADMIN_TOKEN")
	registerToken := mustEnv("REGISTER_TOKEN")
	routerToken := mustEnv("ROUTER_TOKEN")
	dsn := mustEnv("DATABASE_URL")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	store, err := NewStore(ctx, dsn)
	if err != nil {
		fatal("connect postgres failed", "err", err.Error())
	}
	defer store.Close()

	// 不在集群里（本地调试）时开通能力关闭，其它接口照常工作。
	// 设了 DSH_K8S_KUBECONFIG 就在集群外直连 API server，走同一套 Provision 代码（见 k8s_local.go）。
	provCfg := provisionConfigFromEnv()
	var prov *Provisioner
	if p, source, err := newProvisioner(ctx, provCfg); err != nil {
		logger.Warn("provisioning disabled", "reason", err.Error(),
			"hint", "本地调试可设 DSH_K8S_KUBECONFIG=<kubeconfig>")
	} else {
		prov = p
		logger.Info("provisioning enabled", "source", source,
			"namespace", provCfg.Namespace, "baseDomain", provCfg.BaseDomain)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := store.Ping(r.Context()); err != nil {
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// pod 启动时上报路由信息与 launch token；同时把期望状态拉回 running
	mux.HandleFunc("/internal/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || bearer(r) != registerToken {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var rec Record
		if !decode(w, r, &rec) {
			return
		}
		if rec.Handle == "" || rec.Subject == "" || rec.PublicHost == "" || rec.PodIP == "" ||
			rec.PrincipalURI == "" || rec.LaunchToken == "" {
			http.Error(w, "missing field", http.StatusBadRequest)
			return
		}
		if err := store.Put(r.Context(), rec); err != nil {
			logFor(r.Context()).Error("register failed", "err", err.Error())
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		logFor(r.Context()).Info("registered", "handle", rec.Handle, "subject", rec.Subject,
			"host", rec.PublicHost, "pod", rec.PodIP)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// 活动插件心跳
	mux.HandleFunc("/internal/activity", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || bearer(r) != registerToken {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var body struct {
			Subject        string `json:"subject"`
			OpenTurn       bool   `json:"openTurn"`
			LastActivityAt int64  `json:"lastActivityAt"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.Subject == "" {
			http.Error(w, "missing subject", http.StatusBadRequest)
			return
		}
		ok, err := store.Touch(r.Context(), body.Subject, body.OpenTurn, body.LastActivityAt)
		if err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "not registered", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// router 查目标
	mux.HandleFunc("/internal/lookup", func(w http.ResponseWriter, r *http.Request) {
		if bearer(r) != routerToken {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		subject := r.URL.Query().Get("subject")
		if subject == "" {
			http.Error(w, "missing subject", http.StatusBadRequest)
			return
		}
		rec, err := store.Get(r.Context(), subject)
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, rec)
	})

	// 回收器：空闲列表
	mux.HandleFunc("/internal/idle", func(w http.ResponseWriter, r *http.Request) {
		if bearer(r) != routerToken && bearer(r) != adminToken {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		older := 15 * time.Minute
		if v := r.URL.Query().Get("olderThanSeconds"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				older = time.Duration(n) * time.Second
			}
		}
		list, err := store.Idle(r.Context(), older)
		if err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, list)
	})

	// 回收器：需要拉起的用户
	mux.HandleFunc("/internal/wanted", func(w http.ResponseWriter, r *http.Request) {
		if bearer(r) != routerToken && bearer(r) != adminToken {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		list, err := store.Wanted(r.Context())
		if err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, list)
	})

	// 修改期望状态（router 唤醒 / 回收器休眠）
	mux.HandleFunc("/internal/desired", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || (bearer(r) != routerToken && bearer(r) != adminToken) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var body struct {
			Subject      string `json:"subject"`
			DesiredState string `json:"desiredState"`
			ClearPod     bool   `json:"clearPod"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.DesiredState != "running" && body.DesiredState != "sleeping" && body.DesiredState != "archived" {
			http.Error(w, "bad desiredState", http.StatusBadRequest)
			return
		}
		ok, err := store.SetDesired(r.Context(), body.Subject, body.DesiredState)
		if err != nil {
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if body.ClearPod {
			if err := store.ClearPod(r.Context(), body.Subject); err != nil {
				logFor(r.Context()).Error("clear pod failed", "subject", body.Subject, "err", err.Error())
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// 开通：创建 Secret/PVC/Certificate/Deployment 并登记（管理台与 CLI 共用）
	mux.HandleFunc("/internal/provision", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || bearer(r) != adminToken {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if prov == nil {
			http.Error(w, "provisioning unavailable (not in cluster)", http.StatusServiceUnavailable)
			return
		}
		var in ProvisionInput
		if !decode(w, r, &in) {
			return
		}
		in.Actor = r.Header.Get("X-DSH-Actor")
		res, err := prov.Provision(r.Context(), in, store)
		if err != nil {
			status := http.StatusInternalServerError
			var pe *provisionError
			if errors.As(err, &pe) {
				status = pe.Status
			}
			// 阶段日志在 Provision 内部输出，这里只决定 HTTP 语义
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, res)
	})

	// 用户列表（管理台）；POST 保留给老的 CLI 登记用法
	mux.HandleFunc("/internal/users", func(w http.ResponseWriter, r *http.Request) {
		if bearer(r) != adminToken {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodGet:
			list, err := store.List(r.Context())
			if err != nil {
				logFor(r.Context()).Error("list users failed", "err", err.Error())
				http.Error(w, "store error", http.StatusInternalServerError)
				return
			}
			// launch token 等价于 pod 的完整工具权限，管理台列表不需要它。
			for i := range list {
				list[i].LaunchToken = ""
			}
			writeJSON(w, http.StatusOK, list)
		case http.MethodPost:
			var rec Record
			if !decode(w, r, &rec) {
				return
			}
			if rec.Subject == "" || rec.Handle == "" || rec.PublicHost == "" {
				http.Error(w, "missing field", http.StatusBadRequest)
				return
			}
			if err := store.UpsertUser(r.Context(), rec); err != nil {
				logFor(r.Context()).Error("upsert user failed", "err", err.Error())
				http.Error(w, "store error", http.StatusInternalServerError)
				return
			}
			logFor(r.Context()).Info("upserted user", "handle", rec.Handle,
				"subject", rec.Subject, "host", rec.PublicHost)
			writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	srv := &http.Server{Addr: ":" + envOr("PORT", "8080"), Handler: withRequestID(mux), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	logger.Info("dsh-control listening", "addr", srv.Addr, "logLevel", os.Getenv("LOG_LEVEL"))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal("server stopped", "err", err.Error())
	}
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		fatal("missing required env", "key", k)
	}
	return v
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
