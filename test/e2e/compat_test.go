//go:build e2e

// This wraps the P6 workload compatibility fixture (test/e2e/p6-compat/run.sh,
// issue #53) as a Go test so it integrates with `go test` and release gating. It
// is excluded from the default build by the `e2e` tag and additionally gated on
// MACVZ_P6=1, so it only runs when explicitly requested against a real cluster
// with at least one registered macvz-kubelet node:
//
//	MACVZ_P6=1 KUBECONFIG=... go test -tags e2e ./test/e2e/ -run TestP6Compat -v
//
// See test/e2e/p6-compat/README.md for the workload and prerequisites.
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestP6Compat(t *testing.T) {
	if os.Getenv("MACVZ_P6") != "1" {
		t.Skip("set MACVZ_P6=1 (and KUBECONFIG) to run the P6 compatibility fixture")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test source path")
	}
	script := filepath.Join(filepath.Dir(thisFile), "p6-compat", "run.sh")

	cmd := exec.Command("bash", script)
	cmd.Stdout = testWriter{t}
	cmd.Stderr = testWriter{t}
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		t.Fatalf("P6 compatibility fixture failed: %v", err)
	}
}
