package worker

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// Runner 控制面 mTLS(方案 §7):
// - Platform CA 签发两类证书: Runner 短期服务证书(CN 编码 runner_key hash +
//   generation)与 Platform 客户端身份证书。
// - Runner 服务端要求并校验客户端证书(Platform control identity)。
// - Platform 客户端校验服务端链并携带自己的证书。
// - Runner 不持有可调用其他 Runner 的客户端凭据。

const (
	platformClientCN = "ga-platform-control"
	runnerCertCNTmpl = "runner-%s-g%d"
)

// PlatformCA 是 Platform 控制面的短期 CA。
type PlatformCA struct {
	CertPEM []byte
	KeyPEM  []byte

	cert *x509.Certificate
	key  *rsa.PrivateKey
}

// CertMaterial 是签发的证书材料(写入 Runner 或 Platform 本地)。
type CertMaterial struct {
	CertPEM []byte
	KeyPEM  []byte
}

// NewPlatformCA 生成一个新的控制面 CA。
func NewPlatformCA() (*PlatformCA, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate ca key: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ga-runner-control-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create ca cert: %w", err)
	}
	return &PlatformCA{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
		cert:    template,
		key:     key,
	}, nil
}



// IssueRunnerCert 为 (workspace_key, generation) 签发短期服务证书。
// CN 编码 runner_key hash + generation; DNS SAN 包含容器名(runner-control
// 网络内的拨号地址), 供服务端与客户端双重 fencing(方案 §7)。
func (ca *PlatformCA) IssueRunnerCert(workspaceKey string, generation uint64, ttl time.Duration, dnsNames ...string) (CertMaterial, error) {
	if workspaceKey == "" || generation == 0 {
		return CertMaterial{}, fmt.Errorf("workspace key and generation are required")
	}
	cn := fmt.Sprintf(runnerCertCNTmpl, workspaceKey, generation)
	return ca.issue(cn, ttl, x509.ExtKeyUsageServerAuth, dnsNames...)
}

// IssuePlatformClientCert 签发 Platform 控制面的客户端身份证书。
func (ca *PlatformCA) IssuePlatformClientCert(ttl time.Duration) (CertMaterial, error) {
	return ca.issue(platformClientCN, ttl, x509.ExtKeyUsageClientAuth)
}

func (ca *PlatformCA) issue(cn string, ttl time.Duration, extKeyUsage x509.ExtKeyUsage, dnsNames ...string) (CertMaterial, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return CertMaterial{}, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return CertMaterial{}, fmt.Errorf("serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(ttl),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{extKeyUsage},
		DNSNames:     append([]string(nil), dnsNames...),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return CertMaterial{}, fmt.Errorf("create cert %s: %w", cn, err)
	}
	return CertMaterial{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
	}, nil
}

// newCertPool 返回包含 CA 证书的信任池。
func (ca *PlatformCA) newCertPool() *x509.CertPool {
	pool := x509.NewCertPool()
	_ = pool.AppendCertsFromPEM(ca.CertPEM)
	return pool
}

// ServerTLSConfig 构建 Runner 服务端配置: 要求并校验客户端证书(Platform identity)。
func (ca *PlatformCA) ServerTLSConfig(runnerCert CertMaterial) (*tls.Config, error) {
	pair, err := tls.X509KeyPair([]byte(runnerCert.CertPEM), []byte(runnerCert.KeyPEM))
	if err != nil {
		return nil, fmt.Errorf("load runner keypair: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(ca.CertPEM)) {
		return nil, fmt.Errorf("append ca cert")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ClientTLSConfig 构建 Platform 客户端配置: 携带客户端身份证书并校验服务端链。
func (ca *PlatformCA) ClientTLSConfig(platformCert CertMaterial) (*tls.Config, error) {
	pair, err := tls.X509KeyPair([]byte(platformCert.CertPEM), []byte(platformCert.KeyPEM))
	if err != nil {
		return nil, fmt.Errorf("load platform keypair: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(ca.CertPEM)) {
		return nil, fmt.Errorf("append ca cert")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// parseCertPEM 解析测试与校验用的单证书。
func parseCertPEM(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no certificate block")
	}
	return x509.ParseCertificate(block.Bytes)
}

// verifyCertWithPool 校验证书在给定时刻相对池有效。
func verifyCertWithPool(cert *x509.Certificate, pool *x509.CertPool, at time.Time) (*x509.Certificate, error) {
	intermediates := x509.NewCertPool()
	chains, err := cert.Verify(x509.VerifyOptions{
		Roots:         pool,
		Intermediates: intermediates,
		CurrentTime:   at,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	if err != nil {
		return nil, err
	}
	if len(chains) == 0 {
		return nil, fmt.Errorf("no verified chain")
	}
	return chains[0][0], nil
}
