package main

import "testing"

func TestPVCNamedBySlug(t *testing.T) {
	cfg := ProvisionConfig{Namespace: "dsh-users", BaseDomain: "dsh.corp"}
	n := provisionNames(cfg, "bob@corp", slugFor("bob@corp"))
	if n.pvc != "dsh-home-bob" {
		t.Errorf("pvc = %q, want dsh-home-bob（可读名，便于运维直接认出来）", n.pvc)
	}
	if n.deploy != "dsh-bob" || n.secret != "dsh-model-token-bob" || n.cert != "dsh-bob-tls" {
		t.Errorf("其它资源命名漂移了: %+v", n)
	}
}

func TestLegacyPVCNameIsStable(t *testing.T) {
	// 早期版本用 sha256(subject)[:12] 命名。这个值一旦变化，
	// 老用户会被当成"新用户"分配一个空 PVC —— 数据看起来就丢了。
	// alice@corp 的历史 PVC 名，线上可对照 kubectl get pvc 验证。
	if got := legacyPVCName("alice@corp"); got != "dsh-home-244e5ee0cc23" {
		t.Errorf("legacyPVCName(alice@corp) = %q, want dsh-home-244e5ee0cc23", got)
	}
	if legacyPVCName("alice@corp") != legacyPVCName("alice@corp") {
		t.Error("legacyPVCName 必须是纯函数")
	}
}

func TestSlugFor(t *testing.T) {
	cases := map[string]string{
		"alice@corp":     "alice",
		"Bob.Smith@corp": "bob-smith",
		"a_b@corp":       "a-b",
		"--x--@corp":     "x",
	}
	for in, want := range cases {
		if got := slugFor(in); got != want {
			t.Errorf("slugFor(%q) = %q, want %q", in, got, want)
		}
	}
}
