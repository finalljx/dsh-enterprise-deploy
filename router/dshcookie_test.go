package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// newPodServer 起一个带 SPIFFE URI SAN 的 TLS 服务，模拟用户 pod。
// router 的 transportFor 会校验对端证书里的 URI 必须等于 principal。
func newPodServer(t *testing.T, host, principal string, hits *int32) (port string, pool *x509.CertPool) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))

	uri, err := url.Parse(principal)
	if err != nil {
		t.Fatal(err)
	}
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{host},
		URIs:         []*url.URL{uri},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caTmpl, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		// 模拟 DSH：拿 token 换 cookie，回 303 + Set-Cookie
		w.Header().Set("Set-Cookie", "dsh_sess="+r.URL.Query().Get("token"))
		w.WriteHeader(http.StatusSeeOther)
	})}
	tlsLn := tls.NewListener(ln, &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{srvDER}, PrivateKey: srvKey}},
	})
	go func() { _ = srv.Serve(tlsLn) }()
	t.Cleanup(func() { _ = srv.Close() })

	return fmt.Sprint(ln.Addr().(*net.TCPAddr).Port), pool
}

func TestDSHCookieReexchangesOnTokenChange(t *testing.T) {
	// pod 重启会换 launch token，旧 cookie 立即失效。
	// 如果只按 subject 缓存，用户在被回收器唤醒后会一直 401。
	var hits int32
	port, pool := newPodServer(t, "alice.dsh.corp", "spiffe://corp/dsh/user/alice@corp", &hits)

	s := &Server{
		cookieKey: []byte("test-cookie-key"),
		rootCAs:   pool,
		podPort:   port,
	}
	rec := Record{
		Subject:      "alice@corp",
		PublicHost:   "alice.dsh.corp",
		PodIP:        "127.0.0.1",
		PrincipalURI: "spiffe://corp/dsh/user/alice@corp",
		LaunchToken:  "tok-1",
	}
	ctx := context.Background()

	c1, err := s.dshCookie(ctx, rec)
	if err != nil {
		t.Fatalf("第一次换 cookie 失败: %v", err)
	}
	if c1 != "dsh_sess=tok-1" {
		t.Errorf("cookie = %q, want dsh_sess=tok-1", c1)
	}

	// 同一 token：应该直接命中缓存，不再打 pod
	if _, err := s.dshCookie(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("同 token 请求了 %d 次，期望复用缓存只 1 次", got)
	}

	// pod 重启：token 变了，必须重新换
	rec.LaunchToken = "tok-2"
	c2, err := s.dshCookie(ctx, rec)
	if err != nil {
		t.Fatalf("token 变更后换 cookie 失败: %v", err)
	}
	if c2 != "dsh_sess=tok-2" {
		t.Errorf("token 变更后 cookie = %q, want dsh_sess=tok-2（还是旧 cookie 说明缓存没失效）", c2)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("token 变更后累计请求 %d 次，期望 2 次", got)
	}
}
