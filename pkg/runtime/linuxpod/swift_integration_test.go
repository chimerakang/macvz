package linuxpod

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestSwiftHelperStubContract proves the Go HelperClient and the Swift
// LinuxPodHelperStub speak the same CRI-R17 contract over a real unix socket: it
// launches the Swift stub, then runs the exact kubelet-ordering probe (CreatePod,
// app create/start, late sidecar create/start) and asserts shared namespace,
// localhost, and identity verification — the same assertions the in-process and
// over-pipe tests make, now across the language boundary.
//
// Gated behind MACVZ_LINUXPOD_HELPER=1 because it requires the Swift toolchain;
// the default test run proves the contract hermetically with the Go fake. The
// stub binary is taken from MACVZ_LINUXPOD_HELPER_BIN, else built on demand with
// `swift build` in test/e2e/cri-linuxpod-helper.
func TestSwiftHelperStubContract(t *testing.T) {
	if os.Getenv("MACVZ_LINUXPOD_HELPER") != "1" {
		t.Skip("set MACVZ_LINUXPOD_HELPER=1 to run the Go<->Swift LinuxPod helper contract test")
	}

	bin := os.Getenv("MACVZ_LINUXPOD_HELPER_BIN")
	if bin == "" {
		bin = buildSwiftHelper(t)
	}

	// Short socket dir to stay under the unix sun_path limit.
	dir, err := os.MkdirTemp("", "lph")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "h.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--socket", socket)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start swift helper: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	waitForSocket(t, socket)

	client := NewSocketClient(socket)
	app, sidecar, cleanup := orderingProbe(t, client)
	defer cleanup()
	assertSharedNamespaceAndIdentity(t, app, sidecar)
}

// buildSwiftHelper builds the stub and returns its binary path.
func buildSwiftHelper(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file for repo-relative paths")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	helperDir := filepath.Join(repoRoot, "test", "e2e", "cri-linuxpod-helper")

	build := exec.Command("swift", "build", "-c", "debug")
	build.Dir = helperDir
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("swift build helper: %v", err)
	}
	return filepath.Join(helperDir, ".build", "debug", "LinuxPodHelperStub")
}

func waitForSocket(t *testing.T, socket string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(socket); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("swift helper socket %s never appeared", socket)
}
