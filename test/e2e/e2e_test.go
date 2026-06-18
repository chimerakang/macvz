//go:build e2e

// Package e2e wraps the multi-node end-to-end harness (test/e2e/e2e.sh) as a Go
// test so it integrates with `go test` and release gating. It is excluded from
// the default build by the `e2e` tag and additionally gated on MACVZ_E2E=1, so
// it only runs when explicitly requested against a real cluster:
//
//	MACVZ_E2E=1 MACVZ_E2E_NODES=mac-a,mac-b go test -tags e2e ./test/e2e/ -v
//
// See docs/E2E.md for topology and prerequisites.
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMultiNodeE2E(t *testing.T) {
	if os.Getenv("MACVZ_E2E") != "1" {
		t.Skip("set MACVZ_E2E=1 (and MACVZ_E2E_NODES / KUBECONFIG) to run the multi-node e2e suite")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test source path")
	}
	script := filepath.Join(filepath.Dir(thisFile), "e2e.sh")

	cmd := exec.Command("bash", script)
	cmd.Stdout = testWriter{t}
	cmd.Stderr = testWriter{t}
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		t.Fatalf("multi-node e2e suite failed: %v", err)
	}
}

// testWriter streams harness output into the test log line buffer.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}
