// 本地调试用：让 control 在集群外直连 API server，走和集群内完全相同的 Provision 代码。
//
// 为什么不直接读 kubeconfig：它是 YAML，自己解析得引一个 YAML 依赖，
// 而 client-go 的体量对这里「只发三个 REST 请求」的用法不划算（见 provision.go 顶部注释）。
// 调试时 kubectl 本来就在手边，借它把配置归一化成 JSON，只需要 encoding/json。
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// k8sFromKubeconfig 读取指定（或默认）kubeconfig，构造直连 API server 的客户端。
func k8sFromKubeconfig(ctx context.Context, kubeconfig string) (*k8sClient, error) {
	raw, err := kubectlConfigView(ctx, kubeconfig)
	if err != nil {
		return nil, err
	}
	return k8sFromKubeconfigJSON(raw)
}

func kubectlConfigView(ctx context.Context, kubeconfig string) ([]byte, error) {
	args := []string{"config", "view", "--raw", "--minify", "-o", "json"}
	if kubeconfig != "" {
		args = append(args, "--kubeconfig", kubeconfig)
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "kubectl", args...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("kubectl config view: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("kubectl config view: %w", err)
	}
	return out, nil
}

// kubeconfig 只保留这个客户端用得到的字段。
type kubeconfig struct {
	Clusters []struct {
		Name    string `json:"name"`
		Cluster struct {
			Server   string `json:"server"`
			CAData   string `json:"certificate-authority-data"`
			CAFile   string `json:"certificate-authority"`
			Insecure bool   `json:"insecure-skip-tls-verify"`
		} `json:"cluster"`
	} `json:"clusters"`
	Users []struct {
		Name string `json:"name"`
		User struct {
			Token     string `json:"token"`
			TokenFile string `json:"tokenFile"`
			CertData  string `json:"client-certificate-data"`
			KeyData   string `json:"client-key-data"`
			CertFile  string `json:"client-certificate"`
			KeyFile   string `json:"client-key"`
		} `json:"user"`
	} `json:"users"`
}

// k8sFromKubeconfigJSON 是纯函数，便于单测覆盖各种凭据形态。
func k8sFromKubeconfigJSON(raw []byte) (*k8sClient, error) {
	var kc kubeconfig
	if err := json.Unmarshal(raw, &kc); err != nil {
		return nil, fmt.Errorf("parse kubeconfig json: %w", err)
	}
	if len(kc.Clusters) == 0 {
		return nil, errors.New("kubeconfig 里没有 cluster")
	}
	if len(kc.Users) == 0 {
		return nil, errors.New("kubeconfig 里没有 user")
	}

	c := kc.Clusters[0].Cluster
	base := strings.TrimRight(c.Server, "/")
	if base == "" {
		return nil, errors.New("kubeconfig 里 cluster.server 为空")
	}
	// k3d 生成的 server 常是 0.0.0.0:PORT，本机连不上，换成回环地址。
	if strings.Contains(base, "//0.0.0.0:") {
		base = strings.Replace(base, "//0.0.0.0:", "//127.0.0.1:", 1)
	}

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.Insecure}
	if !c.Insecure {
		ca, err := decodeMaybeFile(c.CAData, c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("cluster CA: %w", err)
		}
		if len(ca) == 0 {
			return nil, errors.New("kubeconfig 既没有 CA 也没有 insecure-skip-tls-verify，拒绝明文连接")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("cluster CA 里没有证书")
		}
		tlsCfg.RootCAs = pool
	}

	u := kc.Users[0].User
	token := u.Token
	if token == "" && u.TokenFile != "" {
		b, err := os.ReadFile(u.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("read token file: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	certPEM, err := decodeMaybeFile(u.CertData, u.CertFile)
	if err != nil {
		return nil, fmt.Errorf("client certificate: %w", err)
	}
	keyPEM, err := decodeMaybeFile(u.KeyData, u.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("client key: %w", err)
	}
	if len(certPEM) > 0 && len(keyPEM) > 0 {
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("client keypair: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
	}
	// k3d / minikube 默认是客户端证书，kubeadm 常是 token，两种都支持。
	if token == "" && len(tlsCfg.Certificates) == 0 {
		return nil, errors.New("kubeconfig 里既没有 token 也没有客户端证书（exec 认证插件暂不支持）")
	}

	return &k8sClient{
		base:  base,
		token: token,
		http: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}, nil
}

// decodeMaybeFile 优先用内联的 base64 数据，其次读文件；都没有返回 nil。
func decodeMaybeFile(data, file string) ([]byte, error) {
	if data != "" {
		b, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return nil, fmt.Errorf("base64: %w", err)
		}
		return b, nil
	}
	if file != "" {
		return os.ReadFile(file)
	}
	return nil, nil
}
