package main

// 开通用户：控制面直接调 K8s API 创建 Secret/PVC/Certificate/Deployment，
// 再把记录写进 dsh_users。这是管理员页面与 CLI 共用的唯一实现。
//
// 为什么放在 control：
//   - 04-rbac.yaml 只给 dsh-control 的 ServiceAccount 开了 PVC/Deployment/Certificate 权限；
//   - router 没有、也不该有 K8s 权限，它只负责认证与转发；
//   - 用户资源、数据库记录在同一次请求里创建，失败点集中、可重试。
//
// 这里用 Kubernetes REST API 而不是 client-go：只用到 create/patch/get 三个动作，
// 不值得引入一整套依赖。集群内凭据走 ServiceAccount 的标准路径。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// errNotInCluster 让调用方能把「不在集群里」和其它失败区分开，从而决定要不要
// 退回本地 kubeconfig（见 newProvisioner）。
var errNotInCluster = errors.New("not running in a cluster (KUBERNETES_SERVICE_HOST unset)")

// userRegistrar 是开通流程对存储的最小依赖，便于单测替换。
type userRegistrar interface {
	UpsertUser(ctx context.Context, r Record) error
	HandleOwner(ctx context.Context, handle string) (string, error)
}

// ProvisionConfig 是集群级约定，全部来自控制面的环境变量；管理员页面不填这些。
type ProvisionConfig struct {
	Namespace       string
	BaseDomain      string
	Image           string
	StorageClass    string
	RuntimeClass    string
	Issuer          string
	Storage         string
	ModelEnvName    string
	PrincipalPrefix string
}

func provisionConfigFromEnv() ProvisionConfig {
	return ProvisionConfig{
		Namespace:    envOr("DSH_NAMESPACE", "dsh-users"),
		BaseDomain:   envOr("DSH_BASE_DOMAIN", "dsh.corp"),
		Image:        envOr("DSH_IMAGE", "registry.corp/dsh:0.2.1-alpha.4"),
		StorageClass: envOr("DSH_STORAGE_CLASS", "dsh-rbd"),
		RuntimeClass: envOr("DSH_RUNTIME_CLASS", "kata-clh-runtime-rs"),
		Issuer:       envOr("DSH_ISSUER", "dsh-internal-ca"),
		Storage:      envOr("DSH_STORAGE", "30Gi"),
		// 必须与镜像 profile 里的 apiKeyEnv 一致，否则 key 注入了但 DSH 读不到：
		// pod 里会出现"页面明明设了 key、却报 authentication_error"的现象。
		ModelEnvName:    envOr("DSH_MODEL_ENV_NAME", "DEEPSEEK_API_KEY"),
		PrincipalPrefix: envOr("DSH_PRINCIPAL_PREFIX", "spiffe://corp/dsh/user/"),
	}
}

type ProvisionInput struct {
	Subject    string `json:"subject"`
	Tier       string `json:"tier"`
	ModelToken string `json:"modelToken"`
	Actor      string `json:"-"`
}

type ProvisionResult struct {
	Subject  string   `json:"subject"`
	Handle   string   `json:"handle"`
	Host     string   `json:"host"`
	Warnings []string `json:"warnings,omitempty"`
}

// provisionError 带 HTTP 语义，路由层据此回 400/409/502，而不是一律 500。
type provisionError struct {
	Status int
	Msg    string
}

func (e *provisionError) Error() string { return e.Msg }

func badRequest(format string, args ...any) error {
	return &provisionError{Status: http.StatusBadRequest, Msg: fmt.Sprintf(format, args...)}
}

type Provisioner struct {
	cfg ProvisionConfig
	k8s *k8sClient
}

func NewProvisioner(cfg ProvisionConfig, k8s *k8sClient) *Provisioner {
	return &Provisioner{cfg: cfg, k8s: k8s}
}

// newProvisioner 依次尝试集群内 ServiceAccount 凭据与本地 kubeconfig。
// 两条路都走不通时返回错误，调用方据此关闭开通能力（其余接口照常）。
// 返回的 source 只用于启动日志，便于确认「我到底连的哪个集群」。
func newProvisioner(ctx context.Context, cfg ProvisionConfig) (*Provisioner, string, error) {
	k8s, err := inClusterK8s()
	source := "in-cluster"
	if errors.Is(err, errNotInCluster) {
		if path := envOr("DSH_K8S_KUBECONFIG", ""); path != "" {
			k8s, err = k8sFromKubeconfig(ctx, path)
			source = "kubeconfig=" + path
		}
	}
	if err != nil {
		return nil, source, err
	}
	return NewProvisioner(cfg, k8s), source, nil
}

var subjectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._%+-]{0,63}@[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

// Provision 幂等：重复调用只会 PATCH 已有对象，不会重建、不会动 PVC。
//
// 每个阶段单独记一条日志（step/result/ms），失败时带上 k8s 状态码与 reason，
// 线上排障因此能直接回答「哪一步、新建还是复用、为什么失败」。
func (p *Provisioner) Provision(ctx context.Context, in ProvisionInput, users userRegistrar) (ProvisionResult, error) {
	started := time.Now()
	log := logFor(ctx).With("event", "provision",
		"subject", strings.ToLower(strings.TrimSpace(in.Subject)), "actor", in.Actor)

	res, err := p.provision(ctx, in, users, log)
	if err != nil {
		// 4xx 是调用方问题（输错邮箱、handle 被占），不该混进 Error 告警里。
		level := slog.LevelError
		if status := httpStatusOf(err); status < 500 {
			level = slog.LevelWarn
		}
		log.Log(ctx, level, "provision failed",
			"status", httpStatusOf(err), "ms", time.Since(started).Milliseconds(), "err", err.Error())
		return res, err
	}
	log.Info("provision ok", "handle", res.Handle, "host", res.Host,
		"tier", in.Tier, "ms", time.Since(started).Milliseconds())
	return res, nil
}

func httpStatusOf(err error) int {
	var pe *provisionError
	if errors.As(err, &pe) {
		return pe.Status
	}
	return http.StatusInternalServerError
}

// step 跑一个开通阶段并记日志。out 是 apply 的结论（created/patched/unchanged/reused）。
func step(ctx context.Context, log *slog.Logger, name string, fn func() (string, error)) (string, error) {
	started := time.Now()
	out, err := fn()
	ms := time.Since(started).Milliseconds()
	if err != nil {
		args := []any{"step", name, "ms", ms, "err", err.Error()}
		var api *k8sAPIError
		if errors.As(err, &api) {
			// 403=RBAC、409=冲突、404=资源不存在，分开就能一眼定位。
			args = append(args, "op", api.Op, "status", api.Code, "reason", api.Reason)
		}
		log.Error("provision step failed", args...)
		return out, err
	}
	log.Info("provision step", "step", name, "result", out, "ms", ms)
	return out, nil
}

func (p *Provisioner) provision(ctx context.Context, in ProvisionInput, users userRegistrar, log *slog.Logger) (ProvisionResult, error) {
	subject := strings.ToLower(strings.TrimSpace(in.Subject))
	if !subjectPattern.MatchString(subject) {
		return ProvisionResult{}, badRequest("邮箱格式不对：%q（需要小写，如 alice@corp）", in.Subject)
	}
	slug := slugFor(subject)
	if slug == "" || len(slug) > 40 {
		return ProvisionResult{}, badRequest("无法从 %q 得到合法的 handle（长度需在 1–40）", subject)
	}
	tier := strings.ToUpper(strings.TrimSpace(in.Tier))
	switch tier {
	case "":
		tier = "T1"
	case "T0", "T1", "T2":
	default:
		return ProvisionResult{}, badRequest("tier 只能是 T0/T1/T2")
	}

	owner, err := users.HandleOwner(ctx, slug)
	if err != nil {
		return ProvisionResult{}, err
	}
	if owner != "" && owner != subject {
		return ProvisionResult{},
			&provisionError{Status: http.StatusConflict, Msg: fmt.Sprintf("handle %q 已被 %s 占用，请改用其它邮箱前缀", slug, owner)}
	}

	names := provisionNames(p.cfg, subject, slug)
	result := ProvisionResult{Subject: subject, Handle: slug, Host: names.host}
	log = log.With("handle", slug, "host", names.host)

	if strings.TrimSpace(in.ModelToken) == "" {
		// 这里刻意不把 token 传进闭包，避免它出现在任何日志字段里。
		if _, err := step(ctx, log, "secret", func() (string, error) {
			code, _, err := p.k8s.do(ctx, http.MethodGet, names.secretPath(), nil, "")
			if err != nil {
				return "", err
			}
			if code == http.StatusNotFound {
				return "", badRequest("缺少模型密钥：%s 不存在，请填写 modelToken", names.secret)
			}
			result.Warnings = append(result.Warnings, "沿用已有模型密钥")
			return "reused", nil
		}); err != nil {
			return ProvisionResult{}, err
		}
	} else if _, err := step(ctx, log, "secret", func() (string, error) {
		return p.applySecret(ctx, names, in.ModelToken)
	}); err != nil {
		return ProvisionResult{}, err
	}

	names.pvc = p.resolvePVCName(ctx, names)
	if _, err := step(ctx, log, "pvc", func() (string, error) {
		return p.applyPVC(ctx, names, tier)
	}); err != nil {
		return ProvisionResult{}, err
	}
	if _, err := step(ctx, log, "certificate", func() (string, error) {
		return p.applyCertificate(ctx, names)
	}); err != nil {
		return ProvisionResult{}, err
	}
	if _, err := step(ctx, log, "deployment", func() (string, error) {
		return p.applyDeployment(ctx, names, subject, tier)
	}); err != nil {
		return ProvisionResult{}, err
	}

	if _, err := step(ctx, log, "store", func() (string, error) {
		if err := users.UpsertUser(ctx, Record{
			Subject:      subject,
			Handle:       slug,
			PublicHost:   names.host,
			PrincipalURI: p.cfg.PrincipalPrefix + subject,
		}); err != nil {
			// 资源已建好，登记失败时重试一次即可（所有 apply 都是幂等的）。
			return "", fmt.Errorf("register user: %w", err)
		}
		return "upserted", nil
	}); err != nil {
		return ProvisionResult{}, err
	}
	return result, nil
}

type names struct {
	namespace string
	subject   string
	slug      string
	host      string
	label     string
	pvc       string
	cert      string
	secret    string
	deploy    string
}

func provisionNames(cfg ProvisionConfig, subject, slug string) names {
	return names{
		namespace: cfg.Namespace,
		subject:   subject,
		slug:      slug,
		host:      slug + "." + cfg.BaseDomain,
		label:     labelFor(subject),
		// 可读名：dsh-home-<slug>。subject 的 hash 只用于兼容早期命名（见 legacyPVCName）。
		pvc:    "dsh-home-" + slug,
		cert:   "dsh-" + slug + "-tls",
		secret: "dsh-model-token-" + slug,
		deploy: "dsh-" + slug,
	}
}

// legacyPVCName 是早期版本用的 hash 命名（dsh-home-<sha256(subject)[:12]>）。
// 仅用于兼容已存在的用户：PVC 不能改名，重建会丢掉数据。
func legacyPVCName(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return "dsh-home-" + hex.EncodeToString(sum[:])[:12]
}

// resolvePVCName：新名字不存在但旧名字存在时，继续用旧名字，避免把老用户的数据"弄丢"。
func (p *Provisioner) resolvePVCName(ctx context.Context, n names) string {
	code, _, err := p.k8s.do(ctx, http.MethodGet, p.pvcPath(n.namespace, n.pvc), nil, "")
	if err != nil || code != http.StatusNotFound {
		return n.pvc
	}
	legacy := legacyPVCName(n.subject)
	if legacy == n.pvc {
		return n.pvc
	}
	code, _, err = p.k8s.do(ctx, http.MethodGet, p.pvcPath(n.namespace, legacy), nil, "")
	if err == nil && code == http.StatusOK {
		logFor(ctx).Warn("沿用历史 PVC 命名", "subject", n.subject, "pvc", legacy)
		return legacy
	}
	return n.pvc
}

func (p *Provisioner) pvcPath(namespace, name string) string {
	return "/api/v1/namespaces/" + url.PathEscape(namespace) + "/persistentvolumeclaims/" + url.PathEscape(name)
}

func (n names) secretPath() string {
	return "/api/v1/namespaces/" + url.PathEscape(n.namespace) + "/secrets/" + url.PathEscape(n.secret)
}

func slugFor(subject string) string {
	local := subject
	if i := strings.Index(subject, "@"); i >= 0 {
		local = subject[:i]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(local) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

var labelSanitizer = regexp.MustCompile(`[^a-z0-9_.-]`)

// labelFor 把 subject 转成合法的 label 值：alice@corp -> alice_corp。
func labelFor(subject string) string {
	value := labelSanitizer.ReplaceAllString(strings.ToLower(subject), "_")
	if len(value) > 63 {
		value = value[:63]
	}
	value = strings.Trim(value, "_.-")
	if value == "" {
		return "unknown"
	}
	return value
}

func (p *Provisioner) applySecret(ctx context.Context, n names, token string) (string, error) {
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      n.secret,
			"namespace": n.namespace,
			"labels":    map[string]string{"dsh.corp/user": n.label},
		},
		"type":       "Opaque",
		"stringData": map[string]string{"token": token},
	}
	return p.k8s.apply(ctx, "/api/v1/namespaces/"+url.PathEscape(n.namespace)+"/secrets", n.secret, obj, obj, true)
}

func (p *Provisioner) applyPVC(ctx context.Context, n names, tier string) (string, error) {
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":      n.pvc,
			"namespace": n.namespace,
			"labels":    map[string]string{"dsh.corp/user": n.label, "dsh.corp/tier": tier},
		},
		"spec": map[string]any{
			"accessModes":      []string{"ReadWriteOnce"},
			"storageClassName": p.cfg.StorageClass,
			"resources": map[string]any{
				"requests": map[string]string{"storage": p.cfg.Storage},
			},
		},
	}
	// PVC 的 spec 不可变：已存在就原样保留，避免误改容量或 StorageClass。
	return p.k8s.apply(ctx, "/api/v1/namespaces/"+url.PathEscape(n.namespace)+"/persistentvolumeclaims", n.pvc, obj, nil, false)
}

func (p *Provisioner) applyCertificate(ctx context.Context, n names) (string, error) {
	obj := map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]any{
			"name":      n.cert,
			"namespace": n.namespace,
			"labels":    map[string]string{"dsh.corp/user": n.label},
		},
		"spec": map[string]any{
			"secretName":  n.cert,
			"issuerRef":   map[string]string{"name": p.cfg.Issuer, "kind": "ClusterIssuer"},
			"dnsNames":    []string{n.host},
			"uris":        []string{p.cfg.PrincipalPrefix + n.subject},
			"duration":    "720h",
			"renewBefore": "120h",
		},
	}
	return p.k8s.apply(ctx, "/apis/cert-manager.io/v1/namespaces/"+url.PathEscape(n.namespace)+"/certificates", n.cert, obj, obj, true)
}

func (p *Provisioner) applyDeployment(ctx context.Context, n names, subject, tier string) (string, error) {
	podLabels := map[string]string{
		"app":           "dsh-user",
		"dsh.corp/user": n.label,
		"dsh.corp/tier": tier,
	}
	annotations := map[string]string{
		"dsh.corp/home-claim":  n.pvc,
		"dsh.corp/public-host": n.host,
		"dsh.corp/principal":   subject,
		"dsh.corp/handle":      n.slug,
	}

	podSpec := map[string]any{
		"serviceAccountName":            "dsh-workload",
		"terminationGracePeriodSeconds": 40,
		"securityContext": map[string]any{
			"runAsNonRoot": true,
			"runAsUser":    1000,
			"fsGroup":      1000,
			"seccompProfile": map[string]string{
				"type": "RuntimeDefault",
			},
		},
		"tolerations": []map[string]any{
			{"key": "node.kubernetes.io/unreachable", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": 30},
			{"key": "node.kubernetes.io/not-ready", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": 30},
			{"key": "dsh.corp/dedicated", "operator": "Equal", "value": "dsh", "effect": "NoSchedule"},
		},
		"initContainers": []map[string]any{{
			"name":            "init-home",
			"image":           p.cfg.Image,
			"command":         []string{"/opt/dsh/init-home.sh"},
			"securityContext": map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}},
			"env": []map[string]any{{
				"name": "DSH_USER_ID",
				"valueFrom": map[string]any{"fieldRef": map[string]string{
					"fieldPath": "metadata.annotations['dsh.corp/principal']",
				}},
			}},
			"volumeMounts": []map[string]any{{"name": "home", "mountPath": "/data/dsh"}},
		}},
		"containers": []map[string]any{{
			"name":    "dsh",
			"image":   p.cfg.Image,
			"command": []string{"/opt/dsh/entrypoint.sh"},
			"ports":   []map[string]any{{"containerPort": 8443, "name": "https"}},
			"env": []map[string]any{
				{"name": "DSH_HOME", "value": "/data/dsh"},
				{"name": "DSH_USER_ID", "valueFrom": map[string]any{"fieldRef": map[string]string{"fieldPath": "metadata.annotations['dsh.corp/principal']"}}},
				{"name": "DSH_LABEL", "valueFrom": map[string]any{"fieldRef": map[string]string{"fieldPath": "metadata.labels['dsh.corp/user']"}}},
				{"name": "DSH_HANDLE", "valueFrom": map[string]any{"fieldRef": map[string]string{"fieldPath": "metadata.annotations['dsh.corp/handle']"}}},
				{"name": "POD_IP", "valueFrom": map[string]any{"fieldRef": map[string]string{"fieldPath": "status.podIP"}}},
				{"name": "DSH_PUBLIC_HOST", "valueFrom": map[string]any{"fieldRef": map[string]string{"fieldPath": "metadata.annotations['dsh.corp/public-host']"}}},
				{"name": "PRINCIPAL_URI", "value": p.cfg.PrincipalPrefix + subject},
				{"name": "CONTROL_URL", "value": "http://dsh-control.dsh-system.svc.cluster.local:8080"},
				{"name": "CONTROL_TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]string{"name": "dsh-control-auth", "key": "register-token"}}},
				{"name": p.cfg.ModelEnvName, "valueFrom": map[string]any{"secretKeyRef": map[string]string{"name": n.secret, "key": "token"}}},
			},
			"volumeMounts": []map[string]any{
				{"name": "home", "mountPath": "/data/dsh"},
				{"name": "tls", "mountPath": "/tls", "readOnly": true},
			},
			"resources": map[string]any{
				"requests": map[string]string{"cpu": "500m", "memory": "1Gi"},
				"limits":   map[string]string{"cpu": "4", "memory": "8Gi"},
			},
			"securityContext": map[string]any{"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []string{"ALL"}}},
		}},
		"volumes": []map[string]any{
			{"name": "home", "persistentVolumeClaim": map[string]string{"claimName": n.pvc}},
			{"name": "tls", "secret": map[string]any{"secretName": n.cert, "defaultMode": 0o400}},
		},
	}
	if p.cfg.RuntimeClass != "" && p.cfg.RuntimeClass != "none" {
		podSpec["runtimeClassName"] = p.cfg.RuntimeClass
	}

	obj := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      n.deploy,
			"namespace": n.namespace,
			"labels":    map[string]string{"app": "dsh-user", "dsh.corp/user": n.label},
		},
		"spec": map[string]any{
			"replicas": 0, // 由 reaper 统一拉起，控制面不直接改副本数
			"strategy": map[string]string{"type": "Recreate"},
			"selector": map[string]any{"matchLabels": map[string]string{"app": "dsh-user", "dsh.corp/user": n.label}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": podLabels, "annotations": annotations},
				"spec":     podSpec,
			},
		},
	}
	// 重复开通不能把正在运行的副本数打回 0：PATCH 时不带 spec.replicas，
	// 副本数只由 reaper 管。
	spec := obj["spec"].(map[string]any)
	patchSpec := map[string]any{}
	for key, value := range spec {
		if key != "replicas" {
			patchSpec[key] = value
		}
	}
	patchObj := map[string]any{
		"apiVersion": obj["apiVersion"],
		"kind":       obj["kind"],
		"metadata":   obj["metadata"],
		"spec":       patchSpec,
	}
	return p.k8s.apply(ctx, "/apis/apps/v1/namespaces/"+url.PathEscape(n.namespace)+"/deployments", n.deploy, obj, patchObj, true)
}

// ---- 极简 Kubernetes REST 客户端 ----

type k8sClient struct {
	base  string
	token string
	// tokenPath 非空时每次请求重读：集群里的 ServiceAccount token 是短时的，
	// kubelet 会轮换文件，缓存住会在过期后拿到 401。
	tokenPath string
	http      *http.Client
}

func inClusterK8s() (*k8sClient, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errNotInCluster
	}
	token, err := os.ReadFile(saTokenPath)
	if err != nil {
		return nil, fmt.Errorf("read service account token: %w", err)
	}
	ca, err := os.ReadFile(saCAPath)
	if err != nil {
		return nil, fmt.Errorf("read cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("cluster CA has no certificate")
	}
	return &k8sClient{
		base:      "https://" + host + ":" + port,
		token:     strings.TrimSpace(string(token)),
		tokenPath: saTokenPath,
		http: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				MinVersion: tls.VersionTLS12,
			}},
		},
	}, nil
}

func (k *k8sClient) do(ctx context.Context, method, path string, body any, contentType string) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	token := k.token
	if k.tokenPath != "" {
		raw, err := os.ReadFile(k.tokenPath)
		if err != nil {
			return 0, nil, fmt.Errorf("read service account token: %w", err)
		}
		token = strings.TrimSpace(string(raw))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	started := time.Now()
	resp, err := k.http.Do(req)
	ms := time.Since(started).Milliseconds()
	if err != nil {
		logFor(ctx).Debug("k8s request failed",
			"method", method, "path", path, "ms", ms, "err", err.Error())
		return 0, nil, fmt.Errorf("k8s %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	// 只记方法/路径/状态，不记 body：Secret 的 body 里有模型密钥。
	logFor(ctx).Debug("k8s request",
		"method", method, "path", path, "status", resp.StatusCode, "ms", ms)
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, out, nil
}

// apply 先 GET 再决定 CREATE 还是 PATCH，保证重复点击「新增」不会失败。
// createObj 与 patchObj 分开，是为了让 Deployment 在 PATCH 时省略 replicas 这类不该被覆盖的字段。
// 返回值是实际动作，供阶段日志区分新建与复用（这正是幂等语义的可观测形式）。
func (k *k8sClient) apply(ctx context.Context, collection, name string, createObj, patchObj map[string]any, patchExisting bool) (string, error) {
	item := strings.TrimSuffix(collection, "/") + "/" + url.PathEscape(name)
	code, body, err := k.do(ctx, http.MethodGet, item, nil, "")
	if err != nil {
		return "", err
	}
	switch code {
	case http.StatusNotFound:
		code, body, err = k.do(ctx, http.MethodPost, collection, createObj, "application/json")
		if err != nil {
			return "", err
		}
		if code/100 != 2 {
			return "", k8sError("create", name, code, body)
		}
		return outcomeCreated, nil
	case http.StatusOK:
		if !patchExisting {
			// 已存在且不该覆盖（PVC）：明确记成 unchanged，避免误以为改过。
			return outcomeUnchanged, nil
		}
		code, body, err = k.do(ctx, http.MethodPatch, item, patchObj, "application/merge-patch+json")
		if err != nil {
			return "", err
		}
		if code/100 != 2 {
			return "", k8sError("patch", name, code, body)
		}
		return outcomePatched, nil
	default:
		return "", k8sError("get", name, code, body)
	}
}

const (
	outcomeCreated   = "created"
	outcomePatched   = "patched"
	outcomeUnchanged = "unchanged"
)

// k8sAPIError 保留状态码与 reason，日志里因此能区分 403(RBAC)/404/409(冲突)/422。
type k8sAPIError struct {
	Op     string
	Name   string
	Code   int
	Reason string
	Detail string
}

func (e *k8sAPIError) Error() string {
	return fmt.Sprintf("k8s %s %s failed (%d %s): %s", e.Op, e.Name, e.Code, e.Reason, e.Detail)
}

func k8sError(op, name string, code int, body []byte) error {
	var status struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &status)
	detail := strings.TrimSpace(status.Message)
	if detail == "" {
		detail = strings.TrimSpace(string(body))
	}
	if len(detail) > 300 {
		detail = detail[:300] + "…"
	}
	return &k8sAPIError{Op: op, Name: name, Code: code, Reason: status.Reason, Detail: detail}
}

// Teardown 删除一个用户的工作负载与凭据。
// 顺序刻意从"运行时"往"数据"走：先停 pod（PVC 才能删），再证书、密钥，最后数据卷。
// keepData 为真时保留 PVC —— 适合"先封号、观察一段时间再清理"的流程。
func (p *Provisioner) Teardown(ctx context.Context, subject string, keepData bool) ([]string, error) {
	subject = strings.ToLower(strings.TrimSpace(subject))
	if !subjectPattern.MatchString(subject) {
		return nil, badRequest("邮箱格式不对：%q", subject)
	}
	slug := slugFor(subject)
	n := provisionNames(p.cfg, subject, slug)
	n.pvc = p.resolvePVCName(ctx, n)
	log := logFor(ctx).With("handle", slug, "subject", subject)

	var actions []string
	del := func(op, path, name string) error {
		code, body, err := p.k8s.do(ctx, http.MethodDelete, path, nil, "")
		if err != nil {
			return err
		}
		switch code {
		case http.StatusOK, http.StatusAccepted:
			actions = append(actions, op+" deleted")
		case http.StatusNotFound:
			actions = append(actions, op+" absent")
		default:
			return k8sError(op, name, code, body)
		}
		return nil
	}

	ns := url.PathEscape(n.namespace)
	// Deployment 先删：pod 消失后 PVC 才可能被回收
	if err := del("deployment", "/apis/apps/v1/namespaces/"+ns+"/deployments/"+url.PathEscape(n.deploy), n.deploy); err != nil {
		return actions, err
	}
	if err := del("certificate", "/apis/cert-manager.io/v1/namespaces/"+ns+"/certificates/"+url.PathEscape(n.cert), n.cert); err != nil {
		return actions, err
	}
	// cert-manager 签出来的 TLS Secret 与 Certificate 同名但不会随它删除（没有 ownerReference），
	// 这里显式回收，否则同名 Secret 会一直留在命名空间里。
	if err := del("tls secret", "/api/v1/namespaces/"+ns+"/secrets/"+url.PathEscape(n.cert), n.cert); err != nil {
		return actions, err
	}
	if err := del("secret", n.secretPath(), n.secret); err != nil {
		return actions, err
	}
	if keepData {
		actions = append(actions, "pvc kept")
	} else if err := del("pvc", p.pvcPath(n.namespace, n.pvc), n.pvc); err != nil {
		return actions, err
	}
	log.Info("user torn down", "keepData", keepData, "actions", actions)
	return actions, nil
}
