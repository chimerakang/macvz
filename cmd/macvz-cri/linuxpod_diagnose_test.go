package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chimerakang/macvz/pkg/criserver"
	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime/linuxpod"
)

// TestRunLinuxPodDiagnose proves the standalone `--diagnose-linuxpod` CLI path
// loads persisted records read-only and emits a machine-readable JSON report,
// classifying a Ready record (with no helper socket) as backend-unprobed.
func TestRunLinuxPodDiagnose(t *testing.T) {
	dir := t.TempDir()
	sandboxes, _, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	id, err := store.NewID()
	if err != nil {
		t.Fatalf("store.NewID: %v", err)
	}
	sb := &store.Sandbox{ID: id, State: store.StateReady, CreatedAt: 1}
	sb.Metadata.Namespace = "default"
	sb.Metadata.Name = "pod"
	sb.Metadata.UID = "uid-1"
	if err := sandboxes.Put(sb); err != nil {
		t.Fatalf("persist sandbox: %v", err)
	}
	// A container record in the sibling containers/ dir, exercising the same store
	// layout run() uses.
	containers, _, err := store.NewContainerStore(filepath.Join(dir, "containers"))
	if err != nil {
		t.Fatalf("NewContainerStore: %v", err)
	}
	cid, _ := store.NewID()
	c := &store.Container{ID: cid, SandboxID: id, State: store.ContainerRunning}
	c.Metadata.Name = "app"
	if err := containers.Put(c); err != nil {
		t.Fatalf("persist container: %v", err)
	}

	var out bytes.Buffer
	if err := runLinuxPodDiagnose(context.Background(), &out, dir, linuxpodConfig{}); err != nil {
		t.Fatalf("runLinuxPodDiagnose: %v", err)
	}

	var rep criserver.LinuxPodResidualReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("report is not valid JSON: %v\n%s", err, out.String())
	}
	if rep.BackendProbed {
		t.Error("report should mark backend unprobed when no helper socket is given")
	}
	if len(rep.Sandboxes) != 1 || rep.Sandboxes[0].SandboxID != id {
		t.Fatalf("report sandboxes = %+v, want one sandbox %s", rep.Sandboxes, id)
	}
	if got := rep.Sandboxes[0].Category; got != criserver.ResidualSandboxReadyBackendUnprobed {
		t.Errorf("sandbox category = %q, want %q", got, criserver.ResidualSandboxReadyBackendUnprobed)
	}
	if len(rep.Containers) != 1 || rep.Containers[0].ContainerID != cid {
		t.Fatalf("report containers = %+v, want one container %s", rep.Containers, cid)
	}
	if got := rep.Containers[0].Category; got != criserver.ResidualContainerRunningBackendUnprobed {
		t.Errorf("container category = %q, want %q", got, criserver.ResidualContainerRunningBackendUnprobed)
	}
	if rep.Summary[criserver.ResidualSandboxReadyBackendUnprobed] != 1 {
		t.Errorf("summary unprobed count = %d, want 1", rep.Summary[criserver.ResidualSandboxReadyBackendUnprobed])
	}
	if rep.Summary[criserver.ResidualContainerRunningBackendUnprobed] != 1 {
		t.Errorf("summary container-unprobed count = %d, want 1", rep.Summary[criserver.ResidualContainerRunningBackendUnprobed])
	}
}

func TestRunLinuxPodDiagnoseWithHelperSocketHandshakesAndProbes(t *testing.T) {
	dir := t.TempDir()
	sandboxes, _, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	backend := linuxpod.NewFakeBackend()
	sandboxID, err := store.NewID()
	if err != nil {
		t.Fatalf("store.NewID: %v", err)
	}
	if _, err := backend.CreatePod(context.Background(), linuxpod.PodSpec{ID: sandboxID}); err != nil {
		t.Fatalf("backend CreatePod: %v", err)
	}
	sb := &store.Sandbox{ID: sandboxID, State: store.StateReady, CreatedAt: 1}
	sb.Metadata.Namespace = "default"
	sb.Metadata.Name = "pod"
	if err := sandboxes.Put(sb); err != nil {
		t.Fatalf("persist sandbox: %v", err)
	}

	socket := serveDiagnoseTestBackend(t, backend)
	var out bytes.Buffer
	if err := runLinuxPodDiagnose(context.Background(), &out, dir, linuxpodConfig{helperSocket: socket}); err != nil {
		t.Fatalf("runLinuxPodDiagnose with helper socket: %v", err)
	}

	var rep criserver.LinuxPodResidualReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("report is not valid JSON: %v\n%s", err, out.String())
	}
	if !rep.BackendProbed {
		t.Error("report should mark backend probed when a helper socket is given")
	}
	if len(rep.Sandboxes) != 1 || rep.Sandboxes[0].Category != criserver.ResidualSandboxReadyBackendLive {
		t.Fatalf("report sandboxes = %+v, want one backend-live sandbox", rep.Sandboxes)
	}
}

func TestRunLinuxPodDiagnoseRejectsHelperProtocolMismatch(t *testing.T) {
	dir := t.TempDir()
	socket := serveProtocolMismatchHelper(t)

	var out bytes.Buffer
	err := runLinuxPodDiagnose(context.Background(), &out, dir, linuxpodConfig{helperSocket: socket})
	if err == nil {
		t.Fatal("runLinuxPodDiagnose should reject a mismatched helper protocol")
	}
	if !strings.Contains(err.Error(), "protocol version") || !strings.Contains(err.Error(), "rebuild/restart") {
		t.Fatalf("diagnose protocol mismatch error = %q, want actionable protocol-version guidance", err.Error())
	}
	if out.Len() != 0 {
		t.Fatalf("diagnose wrote a report despite protocol mismatch:\n%s", out.String())
	}
}

func serveDiagnoseTestBackend(t *testing.T, backend linuxpod.Backend) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lpdiag")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "h.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { lis.Close() })
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func() { _ = linuxpod.Serve(context.Background(), conn, backend) }()
		}
	}()
	return socket
}

func serveProtocolMismatchHelper(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lpdiag")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "h.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { lis.Close() })
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadBytes('\n')
		_, _ = fmt.Fprintf(conn,
			"{\"ok\":true,\"result\":{\"name\":\"old-linuxpod-helper\",\"protocolVersion\":%d,\"simulated\":true,\"capabilities\":{}}}\n",
			linuxpod.ProtocolVersion-1)
	}()
	return socket
}
