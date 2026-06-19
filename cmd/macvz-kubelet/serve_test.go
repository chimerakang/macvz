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
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	apiversion "k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
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

func testServingCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Unix(1700000000, 0),
		NotAfter:     time.Unix(1900000000, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
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

func TestWaitForAPIServerSuccess(t *testing.T) {
	client := kubernetesfake.NewClientset()
	client.Discovery().(*fake.FakeDiscovery).FakedServerVersion = &apiversion.Info{GitVersion: "v1.35.0"}

	if err := waitForAPIServerWithTimeout(context.Background(), client, time.Second, 50*time.Millisecond, time.Millisecond); err != nil {
		t.Fatalf("waitForAPIServerWithTimeout: %v", err)
	}
}

func TestWaitForAPIServerFailure(t *testing.T) {
	client := kubernetesfake.NewClientset()
	wantErr := errors.New("no route to host")
	client.Discovery().(*fake.FakeDiscovery).PrependReactor("*", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, wantErr
	})

	err := waitForAPIServerWithTimeout(context.Background(), client, 20*time.Millisecond, 5*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("expected API reachability failure")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want wrapping %v", err, wantErr)
	}
}

func TestListenKubeletTLSRetriesAddressInUse(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen blocker: %v", err)
	}
	addr := blocker.Addr().String()
	released := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = blocker.Close()
		close(released)
	}()

	ln, err := listenKubeletTLSWithRetry(context.Background(), addr, &tls.Config{Certificates: []tls.Certificate{testServingCert(t)}}, time.Second, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("listenKubeletTLSWithRetry: %v", err)
	}
	_ = ln.Close()
	<-released
}

func TestListenKubeletTLSFailsAfterAddressInUseTimeout(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen blocker: %v", err)
	}
	defer func() { _ = blocker.Close() }()

	_, err = listenKubeletTLSWithRetry(context.Background(), blocker.Addr().String(), &tls.Config{Certificates: []tls.Certificate{testServingCert(t)}}, 20*time.Millisecond, 5*time.Millisecond)
	if err == nil {
		t.Fatal("expected address-in-use timeout")
	}
}
