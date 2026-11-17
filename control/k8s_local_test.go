package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 生成一张自签的客户端证书，用来覆盖 k3d/minikube 默认的 mTLS 形态。
func testClientCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "dsh-control-debug"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func kubeconfigJSON(t *testing.T, server, token string, caPEM, certPEM, keyPEM []byte, insecure bool) []byte {
	t.Helper()
	cluster := map[string]any{"server": server, "insecure-skip-tls-verify": insecure}
	if len(caPEM) > 0 {
		cluster["certificate-authority-data"] = base64.StdEncoding.EncodeToString(caPEM)
	}
	user := map[string]any{}
	if token != "" {
		user["token"] = token
	}
	if len(certPEM) > 0 {
		user["client-certificate-data"] = base64.StdEncoding.EncodeToString(certPEM)
		user["client-key-data"] = base64.StdEncoding.EncodeToString(keyPEM)
	}
	raw, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Config",
		"clusters":   []any{map[string]any{"name": "c", "cluster": cluster}},
		"users":      []any{map[string]any{"name": "u", "user": user}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestK8sFromKubeconfigRewritesLoopbackAndKeepsToken(t *testing.T) {
	// k3d 生成的 server 是 0.0.0.0:PORT，本机连不上。
	raw := kubeconfigJSON(t, "https://0.0.0.0:63245", "tok-abc", nil, nil, nil, true)
	c, err := k8sFromKubeconfigJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.base != "https://127.0.0.1:63245" {
		t.Errorf("base = %q, want https://127.0.0.1:63245", c.base)
	}
	if c.token != "tok-abc" {
		t.Errorf("token = %q, want tok-abc", c.token)
	}
}

func TestK8sFromKubeconfigSupportsClientCert(t *testing.T) {
	certPEM, keyPEM := testClientCert(t)
	// CA 用同一张证书即可，这里只验证解析与装载。
	raw := kubeconfigJSON(t, "https://127.0.0.1:6443", "", certPEM, certPEM, keyPEM, false)
	c, err := k8sFromKubeconfigJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg := c.http.Transport.(*http.Transport).TLSClientConfig
	if len(tlsCfg.Certificates) != 1 {
		t.Fatalf("客户端证书数量 = %d, want 1", len(tlsCfg.Certificates))
	}
	if tlsCfg.RootCAs == nil {
		t.Error("RootCAs 未设置")
	}
}

func TestK8sFromKubeconfigRejectsMissingCA(t *testing.T) {
	raw := kubeconfigJSON(t, "https://127.0.0.1:6443", "tok", nil, nil, nil, false)
	_, err := k8sFromKubeconfigJSON(raw)
	if err == nil || !strings.Contains(err.Error(), "CA") {
		t.Fatalf("err = %v, want 缺 CA 的报错", err)
	}
}

func TestK8sFromKubeconfigRejectsMissingCredentials(t *testing.T) {
	raw := kubeconfigJSON(t, "https://127.0.0.1:6443", "", nil, nil, nil, true)
	_, err := k8sFromKubeconfigJSON(raw)
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("err = %v, want 缺凭据的报错", err)
	}
}
