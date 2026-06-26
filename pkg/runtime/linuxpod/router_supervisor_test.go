package linuxpod

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	macvzruntime "github.com/chimerakang/macvz/pkg/runtime"
)

// TestSwiftRouterSupervisorAdoption proves the CRI-L6-4 (#139) ownership inversion
// hermetically: the real linuxpod-helper running as the router spawns a per-Pod
// supervisor, journals it, and routes pod ops to it; a router restart reconnects to
// the surviving supervisor (Adopt -> adopted:true, no recreate); killing the
// supervisor flips Adopt to adopted:false and makes routed status fail (BackendLost
// fallback); and Cleanup terminates the supervisor and drops the journal idempotently.
//
// The supervisor is the in-memory stub (via --supervisor-command), so the whole
// restart/adopt/fallback path is exercised WITHOUT booting a real VM. The same router
// code spawns the real `supervise-pod` VM-owning backend on hardware (see the live
// evidence step in docs/CRI_LINUXPOD_ADOPTION_DESIGN.md).
//
// Gated behind MACVZ_LINUXPOD_HELPER=1 because it requires the Swift toolchain.
func TestSwiftRouterSupervisorAdoption(t *testing.T) {
	if os.Getenv("MACVZ_LINUXPOD_HELPER") != "1" {
		t.Skip("set MACVZ_LINUXPOD_HELPER=1 to run the Go<->Swift router/supervisor adoption test")
	}

	routerBin := buildRouter(t)
	stubBin := buildSwiftHelper(t)

	dir, err := os.MkdirTemp("", "rtr")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "router.sock")
	workDir := filepath.Join(dir, "work")
	const podID = "pod-sup"

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// --- First router instance: create a Pod backed by a stub supervisor. ---
	router := startRouter(t, ctx, routerBin, socket, workDir, stubBin)
	waitForSocket(t, socket)

	client := NewSocketClient(socket)
	info, err := client.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if !info.Adoption.Supported || info.Adoption.AdoptedPods != 0 || info.Adoption.LostPods != 0 {
		t.Fatalf("fresh router adoption state = %+v, want supported with 0/0", info.Adoption)
	}

	pod, err := client.CreatePod(ctx, PodSpec{ID: podID, Hostname: "macvz-sup", CPUs: 2, MemoryBytes: 1 << 30})
	if err != nil {
		t.Fatalf("CreatePod (routed to supervisor): %v", err)
	}
	if pod.Phase != macvzruntime.PhaseRunning || pod.SandboxNamespace == "" {
		t.Fatalf("routed pod not running with a namespace: %+v", pod)
	}
	rootfs, err := client.PrepareContainerRootfs(ctx, RootfsRequest{
		PodID: podID, ContainerName: "app", Image: "busybox", ExpectedIdentity: "macvz-rootfs-id=app",
	})
	if err != nil {
		t.Fatalf("PrepareContainerRootfs (routed): %v", err)
	}
	created, err := client.CreateContainer(ctx, CreateRequest{
		PodID: podID, Name: "app", RootfsToken: rootfs.Token, Command: []string{"/bin/sh", "-c", "sleep 1000"},
	})
	if err != nil {
		t.Fatalf("CreateContainer (routed): %v", err)
	}
	app, err := client.StartContainer(ctx, Ref{PodID: podID, ContainerID: created.ID})
	if err != nil {
		t.Fatalf("StartContainer (routed): %v", err)
	}
	if app.Phase != macvzruntime.PhaseRunning || !app.IdentityVerified {
		t.Fatalf("routed app not running+verified: %+v", app)
	}

	supPID := readSupervisorPID(t, workDir, podID)
	if supPID <= 0 {
		t.Fatalf("journal recorded no supervisor pid for %s", podID)
	}

	// --- Restart the router only; the supervisor must survive. ---
	stopProcess(router)
	router = startRouter(t, ctx, routerBin, socket, workDir, stubBin)
	waitForSocket(t, socket)
	t.Cleanup(func() { stopProcess(router) })

	client = NewSocketClient(socket)
	info, err = client.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping after restart: %v", err)
	}
	if !info.Adoption.Supported || info.Adoption.AdoptedPods != 1 || info.Adoption.LostPods != 0 {
		t.Fatalf("post-restart adoption state = %+v, want 1 adopted / 0 lost", info.Adoption)
	}
	adopt, err := client.Adopt(ctx, podID)
	if err != nil {
		t.Fatalf("Adopt after restart: %v", err)
	}
	if !adopt.Adopted || len(adopt.Containers) == 0 {
		t.Fatalf("Adopt did not reattach the running pod: %+v", adopt)
	}
	if st, err := client.Status(ctx, Ref{PodID: podID, ContainerID: app.ID}); err != nil {
		t.Fatalf("Status routed to adopted supervisor: %v", err)
	} else if st.Phase != macvzruntime.PhaseRunning {
		t.Fatalf("adopted container not still running: %+v", st)
	}

	// --- Kill the supervisor: Adopt must fall back, routed status must fail. ---
	_ = syscall.Kill(supPID, syscall.SIGKILL)
	waitForExit(supPID)

	adopt, err = client.Adopt(ctx, podID)
	if err != nil {
		t.Fatalf("Adopt after supervisor death: unexpected error %v", err)
	}
	if adopt.Adopted {
		t.Fatalf("Adopt must return adopted:false after supervisor death: %+v", adopt)
	}
	if _, err := client.Status(ctx, Ref{PodID: podID, ContainerID: app.ID}); err == nil {
		t.Fatalf("routed Status must fail once the supervisor is dead (BackendLost signal)")
	}

	// --- Cleanup is idempotent and drops the journal entry. ---
	if _, err := client.Cleanup(ctx, podID); err != nil {
		t.Fatalf("Cleanup after supervisor death: %v", err)
	}
	if _, err := client.Cleanup(ctx, podID); err != nil {
		t.Fatalf("second Cleanup must be idempotent: %v", err)
	}
	if pid := readSupervisorPID(t, workDir, podID); pid != 0 {
		t.Fatalf("journal still records supervisor %d after cleanup", pid)
	}
}

// buildRouter builds the real linuxpod-helper (router + supervise-pod) and returns its
// binary path. This compiles the vendored Containerization package, so it is slow on a
// cold cache; the test is gated behind MACVZ_LINUXPOD_HELPER=1 for that reason.
func buildRouter(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file for repo-relative paths")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	helperDir := filepath.Join(repoRoot, "test", "e2e", "cri-linuxpod")

	build := exec.Command("swift", "build", "-c", "debug", "--product", "linuxpod-helper")
	build.Dir = helperDir
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("swift build linuxpod-helper: %v", err)
	}
	return filepath.Join(helperDir, ".build", "debug", "linuxpod-helper")
}

// startRouter launches the router with the stub binary as the per-Pod supervisor
// command, so spawned supervisors model a Pod in memory (no VM boot).
func startRouter(t *testing.T, ctx context.Context, routerBin, socket, workDir, supervisorBin string) *exec.Cmd {
	t.Helper()
	// A killed router leaves its socket file on disk; remove it so waitForSocket only
	// returns once this router has rebound, not on the stale predecessor inode.
	_ = os.Remove(socket)
	cmd := exec.CommandContext(ctx, routerBin, "serve",
		"--socket", socket, "--work-dir", workDir, "--supervisor-command", supervisorBin)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start router: %v", err)
	}
	return cmd
}

// stopProcess kills only the given process (the router) and reaps it, leaving any
// supervisor it spawned alive — the survival the adoption path depends on.
func stopProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}

func waitForExit(pid int) {
	for i := 0; i < 100; i++ {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// readSupervisorPID reads the router's supervisor journal and returns the recorded
// supervisor pid for podID, or 0 when there is no entry.
func readSupervisorPID(t *testing.T, workDir, podID string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workDir, "supervisor-journal.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		t.Fatalf("read supervisor journal: %v", err)
	}
	var journal struct {
		Pods map[string]struct {
			PID int `json:"pid"`
		} `json:"pods"`
	}
	if err := json.Unmarshal(data, &journal); err != nil {
		t.Fatalf("decode supervisor journal: %v", err)
	}
	return journal.Pods[podID].PID
}
