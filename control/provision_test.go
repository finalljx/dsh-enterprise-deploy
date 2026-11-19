package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
)

type fakeRegistrar struct {
	owners  map[string]string
	records []Record
}

func (f *fakeRegistrar) HandleOwner(_ context.Context, handle string) (string, error) {
	return f.owners[handle], nil
}

func (f *fakeRegistrar) UpsertUser(_ context.Context, r Record) error {
	f.records = append(f.records, r)
	return nil
}

type fakeK8s struct {
	mu      sync.Mutex
	objects map[string]map[string]any
	posts   int
	patches int
}

func newFakeK8s() *fakeK8s {
	return &fakeK8s{objects: map[string]map[string]any{}}
}

func (f *fakeK8s) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	switch r.Method {
	case http.MethodGet:
		obj, ok := f.objects[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, obj)
	case http.MethodPost:
		var obj map[string]any
		if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		name, _ := obj["metadata"].(map[string]any)["name"].(string)
		f.objects[path+"/"+name] = obj
		f.posts++
		writeJSON(w, http.StatusCreated, obj)
	case http.MethodPatch:
		var patch map[string]any
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		obj, ok := f.objects[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mergeMap(obj, patch)
		f.patches++
		writeJSON(w, http.StatusOK, obj)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func mergeMap(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				mergeMap(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

func (f *fakeK8s) get(path string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objects[path]
}

func (f *fakeK8s) counts() (posts, patches int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts, f.patches
}

func testConfig() ProvisionConfig {
	return ProvisionConfig{
		Namespace:       "dsh-users",
		BaseDomain:      "dsh.corp",
		Image:           "registry.corp/dsh:test",
		StorageClass:    "dsh-rbd",
		RuntimeClass:    "kata-clh-runtime-rs",
		Issuer:          "dsh-internal-ca",
		Storage:         "30Gi",
		ModelEnvName:    "GATEWAY_TOKEN", // 故意用非默认名，验证配置真的被采用
		PrincipalPrefix: "spiffe://corp/dsh/user/",
	}
}

func newTestProvisioner(t *testing.T, f *fakeK8s) *Provisioner {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return NewProvisioner(testConfig(), &k8sClient{base: srv.URL, token: "test", http: srv.Client()})
}

const (
	deployPath  = "/apis/apps/v1/namespaces/dsh-users/deployments/dsh-alice"
	secretPath  = "/api/v1/namespaces/dsh-users/secrets/dsh-model-token-alice"
	certPath    = "/apis/cert-manager.io/v1/namespaces/dsh-users/certificates/dsh-alice-tls"
	pvcCollPath = "/api/v1/namespaces/dsh-users/persistentvolumeclaims"
)

func hash12(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

func wantProvisionError(t *testing.T, err error, status int) {
	t.Helper()
	var pe *provisionError
	if !errors.As(err, &pe) || pe.Status != status {
		t.Fatalf("err = %v, want status %d", err, status)
	}
}

func TestProvisionCreatesResources(t *testing.T) {
	k8s := newFakeK8s()
	reg := &fakeRegistrar{owners: map[string]string{}}
	p := newTestProvisioner(t, k8s)

	res, err := p.Provision(context.Background(), ProvisionInput{
		Subject: "Alice@Corp ", Tier: "", ModelToken: "sk-test", Actor: "ops@corp",
	}, reg)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.Handle != "alice" || res.Host != "alice.dsh.corp" {
		t.Fatalf("result = %+v", res)
	}
	if k8s.get(secretPath) == nil {
		t.Fatal("model secret not created")
	}
	if k8s.get(certPath) == nil {
		t.Fatal("certificate not created")
	}
	dep := k8s.get(deployPath)
	if dep == nil {
		t.Fatal("deployment not created")
	}
	if len(reg.records) != 1 {
		t.Fatalf("records = %+v", reg.records)
	}
	rec := reg.records[0]
	if rec.Subject != "alice@corp" || rec.Handle != "alice" ||
		rec.PublicHost != "alice.dsh.corp" || rec.PrincipalURI != "spiffe://corp/dsh/user/alice@corp" {
		t.Fatalf("record = %+v", rec)
	}

	spec := dep["spec"].(map[string]any)
	if spec["replicas"].(float64) != 0 {
		t.Fatalf("replicas = %v, want 0 (reaper owns scaling)", spec["replicas"])
	}
	tmpl := spec["template"].(map[string]any)
	ann := tmpl["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann["dsh.corp/public-host"] != "alice.dsh.corp" || ann["dsh.corp/principal"] != "alice@corp" {
		t.Fatalf("annotations = %+v", ann)
	}
	if ann["dsh.corp/home-claim"] != "dsh-home-alice" {
		t.Fatalf("home-claim = %v", ann["dsh.corp/home-claim"])
	}
	containers := tmpl["spec"].(map[string]any)["containers"].([]any)
	env := containers[0].(map[string]any)["env"].([]any)
	var modelRef, principal string
	for _, e := range env {
		m := e.(map[string]any)
		switch m["name"] {
		case "GATEWAY_TOKEN":
			modelRef = m["valueFrom"].(map[string]any)["secretKeyRef"].(map[string]any)["name"].(string)
		case "PRINCIPAL_URI":
			principal = m["value"].(string)
		}
	}
	if modelRef != "dsh-model-token-alice" {
		t.Fatalf("model secret ref = %q", modelRef)
	}
	if principal != "spiffe://corp/dsh/user/alice@corp" {
		t.Fatalf("principal uri = %q", principal)
	}
}

func TestProvisionIsIdempotent(t *testing.T) {
	k8s := newFakeK8s()
	reg := &fakeRegistrar{owners: map[string]string{}}
	p := newTestProvisioner(t, k8s)
	in := ProvisionInput{Subject: "alice@corp", ModelToken: "sk-test"}

	if _, err := p.Provision(context.Background(), in, reg); err != nil {
		t.Fatal(err)
	}
	posts, patches := k8s.counts()
	if _, err := p.Provision(context.Background(), in, reg); err != nil {
		t.Fatal(err)
	}
	posts2, patches2 := k8s.counts()
	if posts2 != posts {
		t.Fatalf("second run created objects again: posts %d -> %d", posts, posts2)
	}
	if patches2 != patches+3 { // secret + certificate + deployment；PVC 已存在时不动
		t.Fatalf("patches = %d, want %d", patches2, patches+3)
	}
	if len(reg.records) != 2 {
		t.Fatalf("upsert calls = %d", len(reg.records))
	}
}

func TestProvisionRequiresModelTokenOnFirstRun(t *testing.T) {
	p := newTestProvisioner(t, newFakeK8s())
	_, err := p.Provision(context.Background(), ProvisionInput{Subject: "alice@corp"}, &fakeRegistrar{})
	wantProvisionError(t, err, http.StatusBadRequest)
}

func TestProvisionReusesExistingSecretWithoutToken(t *testing.T) {
	k8s := newFakeK8s()
	k8s.objects[secretPath] = map[string]any{"metadata": map[string]any{"name": "dsh-model-token-alice"}}
	reg := &fakeRegistrar{}
	p := newTestProvisioner(t, k8s)

	res, err := p.Provision(context.Background(), ProvisionInput{Subject: "alice@corp"}, reg)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected a warning about reusing the secret")
	}
	if len(reg.records) != 1 {
		t.Fatal("user not registered")
	}
}

func TestProvisionRejectsHandleConflict(t *testing.T) {
	p := newTestProvisioner(t, newFakeK8s())
	reg := &fakeRegistrar{owners: map[string]string{"alice": "alice@other"}}
	_, err := p.Provision(context.Background(), ProvisionInput{Subject: "alice@corp", ModelToken: "x"}, reg)
	wantProvisionError(t, err, http.StatusConflict)
}

func TestProvisionValidatesInput(t *testing.T) {
	p := newTestProvisioner(t, newFakeK8s())
	cases := []ProvisionInput{
		{Subject: "not-an-email", ModelToken: "x"},
		{Subject: "alice@corp", Tier: "T9", ModelToken: "x"},
	}
	for _, in := range cases {
		if _, err := p.Provision(context.Background(), in, &fakeRegistrar{}); err == nil {
			t.Fatalf("%+v should fail", in)
		}
	}
}

func TestSlugAndLabelMatchProvisionPy(t *testing.T) {
	if got := slugFor("alice@corp"); got != "alice" {
		t.Fatalf("slug = %q", got)
	}
	if got := slugFor("li.lei+test@corp"); got != "li-lei-test" {
		t.Fatalf("slug = %q", got)
	}
	if got := labelFor("alice@corp"); got != "alice_corp" {
		t.Fatalf("label = %q", got)
	}
}

func TestPVCNotPatchedOnRerun(t *testing.T) {
	k8s := newFakeK8s()
	p := newTestProvisioner(t, k8s)
	in := ProvisionInput{Subject: "bob@corp", ModelToken: "x"}
	if _, err := p.Provision(context.Background(), in, &fakeRegistrar{}); err != nil {
		t.Fatal(err)
	}
	pvcPath := pvcCollPath + "/dsh-home-bob" // 现在按 slug 命名（可读）
	before := k8s.get(pvcPath)
	if before == nil {
		t.Fatal("pvc missing")
	}
	if _, err := p.Provision(context.Background(), in, &fakeRegistrar{}); err != nil {
		t.Fatal(err)
	}
	if k8s.get(pvcPath)["metadata"].(map[string]any)["labels"].(map[string]any) == nil {
		t.Fatal("pvc labels lost")
	}
}

func TestReprovisionKeepsReplicaCount(t *testing.T) {
	k8s := newFakeK8s()
	p := newTestProvisioner(t, k8s)
	in := ProvisionInput{Subject: "alice@corp", ModelToken: "sk-test"}
	if _, err := p.Provision(context.Background(), in, &fakeRegistrar{}); err != nil {
		t.Fatal(err)
	}
	// 模拟 reaper 已经拉起
	dep := k8s.get(deployPath)
	dep["spec"].(map[string]any)["replicas"] = float64(1)

	if _, err := p.Provision(context.Background(), in, &fakeRegistrar{}); err != nil {
		t.Fatal(err)
	}
	if got := k8s.get(deployPath)["spec"].(map[string]any)["replicas"]; got != float64(1) {
		t.Fatalf("replicas = %v, want 1 (re-provision must not scale down)", got)
	}
}

func TestK8sClientRefreshesTokenFromDisk(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	path := t.TempDir() + "/token"
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	k := &k8sClient{base: srv.URL, tokenPath: path, http: srv.Client()}
	if _, _, err := k.do(context.Background(), http.MethodGet, "/x", nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := k.do(context.Background(), http.MethodGet, "/x", nil, ""); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "Bearer first" || seen[1] != "Bearer second" {
		t.Fatalf("authorization headers = %v", seen)
	}
}
