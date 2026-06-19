package bootstrap

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// GenerateServingTLS writes a self-signed kubelet serving certificate and key
// to certPath/keyPath, with the node's InternalIP in the SAN so the API server
// accepts it for kubectl logs/exec. It mirrors what prep-node.sh does with
// openssl, but in-process so `macvz-kubelet bootstrap --gen-tls` needs no extra
// tooling. validFor bounds the certificate lifetime.
func GenerateServingTLS(nodeName, internalIP, certPath, keyPath string, validFor time.Duration, now time.Time) error {
	ip := net.ParseIP(internalIP)
	if ip == nil {
		return fmt.Errorf("internal IP %q is not a valid IP address", internalIP)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generate RSA key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate serial: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: nodeName},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validFor),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{ip},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}

	if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
		return err
	}
	if err := writePEM(keyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key), 0o600); err != nil {
		return err
	}
	return nil
}

func writePEM(path, blockType string, der []byte, mode os.FileMode) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create dir for %s: %w", path, err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		return fmt.Errorf("encode PEM to %s: %w", path, err)
	}
	return f.Close()
}
