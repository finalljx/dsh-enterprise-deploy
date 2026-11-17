package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureLogs 把默认 logger 换成写进 buffer 的 JSON handler，
// 返回读取原始输出的函数。测试结束后恢复，避免影响其它用例。
func captureLogs(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() string { return buf.String() }
}

// entries 把按行输出的 JSON 日志解析成结构，便于断言字段而不是匹配字符串。
func entries(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("日志行不是 JSON: %v (%s)", err, line)
		}
		out = append(out, m)
	}
	return out
}

func stepEntry(all []map[string]any, step string) map[string]any {
	for _, e := range all {
		if e["msg"] == "provision step" && e["step"] == step {
			return e
		}
	}
	return nil
}

func TestProvisionLogsEveryStep(t *testing.T) {
	raw := captureLogs(t)
	f := newFakeK8s()
	p := newTestProvisioner(t, f)

	ctx := context.WithValue(context.Background(), ctxKeyRequestID, "req-steps")
	if _, err := p.Provision(ctx, ProvisionInput{Subject: "alice@corp", Tier: "T1", ModelToken: "sk-x"}, &fakeRegistrar{owners: map[string]string{}}); err != nil {
		t.Fatal(err)
	}

	all := entries(t, raw())
	for _, step := range []string{"secret", "pvc", "certificate", "deployment", "store"} {
		e := stepEntry(all, step)
		if e == nil {
			t.Fatalf("缺少 %s 阶段日志:\n%s", step, raw())
		}
		if e["req"] != "req-steps" {
			t.Errorf("%s 阶段缺 req 字段: %v", step, e)
		}
		if _, ok := e["ms"]; !ok {
			t.Errorf("%s 阶段缺耗时 ms", step)
		}
	}
	// 首次开通应该全部是 created/upserted，这个字段就是幂等语义的可观测形式。
	if got := stepEntry(all, "deployment")["result"]; got != outcomeCreated {
		t.Errorf("deployment result = %v, want %s", got, outcomeCreated)
	}
	if got := stepEntry(all, "store")["result"]; got != "upserted" {
		t.Errorf("store result = %v, want upserted", got)
	}
	if stepEntry(all, "pvc")["result"] != outcomeCreated {
		t.Errorf("pvc result = %v", stepEntry(all, "pvc")["result"])
	}

	var okEntry map[string]any
	for _, e := range all {
		if e["msg"] == "provision ok" {
			okEntry = e
		}
	}
	if okEntry == nil || okEntry["handle"] != "alice" {
		t.Errorf("缺少 provision ok 汇总日志: %v", okEntry)
	}
}

func TestProvisionLogsReuseAndPatchOnRerun(t *testing.T) {
	f := newFakeK8s()
	p := newTestProvisioner(t, f)
	reg := &fakeRegistrar{owners: map[string]string{}}

	if _, err := p.Provision(context.Background(), ProvisionInput{Subject: "alice@corp", ModelToken: "sk-x"}, reg); err != nil {
		t.Fatal(err)
	}

	raw := captureLogs(t)
	// 第二次不带 token：Secret 走「沿用」分支，其它对象应 PATCH。
	if _, err := p.Provision(context.Background(), ProvisionInput{Subject: "alice@corp"}, reg); err != nil {
		t.Fatal(err)
	}
	all := entries(t, raw())
	if got := stepEntry(all, "secret")["result"]; got != "reused" {
		t.Errorf("secret result = %v, want reused", got)
	}
	if got := stepEntry(all, "certificate")["result"]; got != outcomePatched {
		t.Errorf("certificate result = %v, want %s", got, outcomePatched)
	}
	// PVC 的 spec 不可变，已有就原样保留。
	if got := stepEntry(all, "pvc")["result"]; got != outcomeUnchanged {
		t.Errorf("pvc result = %v, want %s", got, outcomeUnchanged)
	}
}

// 线上最需要区分的一种失败：ServiceAccount 少了 verbs。
func TestProvisionLogsK8sStatusOnForbidden(t *testing.T) {
	raw := captureLogs(t)
	f := newFakeK8s()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/deployments") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"kind":"Status","reason":"Forbidden","message":"deployments.apps is forbidden: cannot create resource \"deployments\""}`))
			return
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	p := NewProvisioner(testConfig(), &k8sClient{base: srv.URL, token: "t", http: srv.Client()})
	_, err := p.Provision(context.Background(), ProvisionInput{Subject: "alice@corp", ModelToken: "sk-x"}, &fakeRegistrar{owners: map[string]string{}})
	if err == nil {
		t.Fatal("期望 403 失败")
	}
	var apiErr *k8sAPIError
	if !errors.As(err, &apiErr) || apiErr.Code != http.StatusForbidden || apiErr.Reason != "Forbidden" {
		t.Fatalf("err = %#v, want 403 Forbidden", err)
	}

	var failed map[string]any
	for _, e := range entries(t, raw()) {
		if e["msg"] == "provision step failed" {
			failed = e
		}
	}
	if failed == nil {
		t.Fatalf("缺少失败阶段日志:\n%s", raw())
	}
	if failed["step"] != "deployment" || failed["status"] != float64(http.StatusForbidden) {
		t.Errorf("失败日志字段不对: %v", failed)
	}
	if failed["reason"] != "Forbidden" {
		t.Errorf("reason 字段 = %v, want Forbidden", failed["reason"])
	}
}

func TestProvisionNeverLogsModelToken(t *testing.T) {
	raw := captureLogs(t)
	p := newTestProvisioner(t, newFakeK8s())
	const secret = "sk-do-not-log-me-0123456789"

	if _, err := p.Provision(context.Background(),
		ProvisionInput{Subject: "alice@corp", ModelToken: secret}, &fakeRegistrar{owners: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	if out := raw(); strings.Contains(out, secret) {
		t.Fatalf("模型密钥出现在日志里:\n%s", out)
	}
}

// 4xx 是调用方输错，不该打 Error 触发告警。
func TestProvisionLogsClientErrorAsWarn(t *testing.T) {
	raw := captureLogs(t)
	p := newTestProvisioner(t, newFakeK8s())
	_, err := p.Provision(context.Background(), ProvisionInput{Subject: "not-an-email"}, &fakeRegistrar{owners: map[string]string{}})
	if err == nil {
		t.Fatal("期望参数校验失败")
	}
	var found bool
	for _, e := range entries(t, raw()) {
		if e["msg"] == "provision failed" {
			found = true
			if e["level"] != "WARN" {
				t.Errorf("level = %v, want WARN", e["level"])
			}
			if e["status"] != float64(http.StatusBadRequest) {
				t.Errorf("status = %v, want 400", e["status"])
			}
		}
	}
	if !found {
		t.Fatalf("缺少 provision failed 日志:\n%s", raw())
	}
}
