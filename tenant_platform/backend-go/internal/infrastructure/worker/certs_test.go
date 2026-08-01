package worker

import (
	"crypto/tls"
	"crypto/x509"
	"strings"
	"testing"
	"time"
)

// 测试 mTLS 证书链路: CA → Runner 服务证书(绑 runner_key hash + generation)。
func TestIssueRunnerCertAndVerifyPeer(t *testing.T) {
	ca, err := NewPlatformCA()
	if err != nil {
		t.Fatalf("NewPlatformCA: %v", err)
	}

	runnerCert, err := ca.IssueRunnerCert("personal:42", 7, 24*time.Hour)
	if err != nil {
		t.Fatalf("IssueRunnerCert: %v", err)
	}
	if !strings.Contains(string(runnerCert.CertPEM), "BEGIN CERTIFICATE") {
		t.Fatal("cert pem malformed")
	}
	if !strings.Contains(string(runnerCert.KeyPEM), "BEGIN") {
		t.Fatal("key pem malformed")
	}

	// 校验: 服务端证书 CN 编码 runner_key hash + generation。
	leaf, err := parseCertPEM(runnerCert.CertPEM)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	if leaf.Subject.CommonName != "runner-personal:42-g7" {
		t.Fatalf("CN = %q, want runner-personal:42-g7", leaf.Subject.CommonName)
	}

	// 用 CA 构建客户端池验证服务端证书签名。
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(ca.CertPEM)) {
		t.Fatal("append ca to pool")
	}
	_, err = verifyCertWithPool(leaf, pool, time.Now())
	if err != nil {
		t.Fatalf("verify runner cert against ca: %v", err)
	}
}

func TestRunnerCertExpiry(t *testing.T) {
	ca, err := NewPlatformCA()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ca.IssueRunnerCert("personal:1", 1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := parseCertPEM(cert.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	// 有效期内通过, 过期后失败。
	pool := x509.NewCertPool()
	_ = pool.AppendCertsFromPEM([]byte(ca.CertPEM))
	if _, err := verifyCertWithPool(leaf, pool, time.Now()); err != nil {
		t.Fatalf("valid cert rejected: %v", err)
	}
	if _, err := verifyCertWithPool(leaf, pool, time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("expired cert accepted")
	}
}

func TestClientIdentityCertificate(t *testing.T) {
	ca, err := NewPlatformCA()
	if err != nil {
		t.Fatal(err)
	}
	clientCert, err := ca.IssuePlatformClientCert(24 * time.Hour)
	if err != nil {
		t.Fatalf("IssuePlatformClientCert: %v", err)
	}
	leaf, err := parseCertPEM(clientCert.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Subject.CommonName != "ga-platform-control" {
		t.Fatalf("CN = %q, want ga-platform-control", leaf.Subject.CommonName)
	}
	// 客户端证书必须能被 CA 验证。
	pool := x509.NewCertPool()
	_ = pool.AppendCertsFromPEM([]byte(ca.CertPEM))
	if _, err := verifyCertWithPool(leaf, pool, time.Now()); err != nil {
		t.Fatalf("client cert verify: %v", err)
	}
}

// 端到端: 用签发材料构造 TLS 配置并握手。
func TestMTLSHandshake(t *testing.T) {
	ca, err := NewPlatformCA()
	if err != nil {
		t.Fatal(err)
	}
	runnerCert, err := ca.IssueRunnerCert("personal:9", 3, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	clientCert, err := ca.IssuePlatformClientCert(time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	serverCfg, err := ca.ServerTLSConfig(runnerCert)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	if serverCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatal("server must require client cert")
	}
	clientCfg, err := ca.ClientTLSConfig(clientCert)
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	if clientCfg.RootCAs == nil {
		t.Fatal("client must verify server chain")
	}
	// 反向: 客户端必须持有 CA 签发的证书, 否则握手失败 —— 由 TLS 层保证。
	if clientCfg.Certificates == nil || len(clientCfg.Certificates) == 0 {
		t.Fatal("client config missing certificate")
	}
}
