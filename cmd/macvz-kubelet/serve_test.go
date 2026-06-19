package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTestCA writes a self-signed CA certificate to a temp file and returns its
// path, for exercising the kubelet server's client-CA wiring.
func writeTestCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Unix(1700000000, 0),
		NotAfter:              time.Unix(1900000000, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	return path
}

func TestBuildServingTLSConfigNoClientCA(t *testing.T) {
	cfg, err := buildServingTLSConfig(tls.Certificate{}, "")
	if err != nil {
		t.Fatalf("buildServingTLSConfig: %v", err)
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Errorf("ClientAuth = %v, want NoClientCert when no CA configured", cfg.ClientAuth)
	}
	if cfg.ClientCAs != nil {
		t.Error("ClientCAs should be nil without a configured CA")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2", cfg.MinVersion)
	}
}

func TestBuildServingTLSConfigRequiresClientCert(t *testing.T) {
	ca := writeTestCA(t)
	cfg, err := buildServingTLSConfig(tls.Certificate{}, ca)
	if err != nil {
		t.Fatalf("buildServingTLSConfig: %v", err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert (mutual TLS)", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Error("ClientCAs pool should be populated from the CA file")
	}
}

func TestBuildServingTLSConfigRejectsBadCA(t *testing.T) {
	if _, err := buildServingTLSConfig(tls.Certificate{}, "/nonexistent/ca.pem"); err == nil {
		t.Error("expected error for missing CA file")
	}

	garbage := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(garbage, []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := buildServingTLSConfig(tls.Certificate{}, garbage); err == nil {
		t.Error("expected error for a CA file with no usable certificates")
	}
}
