package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chimerakang/macvz/pkg/runtime/linuxpod"
)

// TestLinuxPodConfigValidate covers the gate's honest configuration checks: the
// backend is a no-op when off, and enabling it without a helper socket is a loud
// error rather than a silent ignore.
func TestLinuxPodConfigValidate(t *testing.T) {
	if err := (linuxpodConfig{}).validate(); err != nil {
		t.Errorf("disabled config should validate, got %v", err)
	}
	err := (linuxpodConfig{enabled: true}).validate()
	if err == nil || !strings.Contains(err.Error(), "--linuxpod-helper-socket") {
		t.Errorf("enabled without socket should fail naming the socket flag, got %v", err)
	}
	if err := (linuxpodConfig{enabled: true, helperSocket: "/tmp/x.sock"}).validate(); err != nil {
		t.Errorf("enabled with socket should validate, got %v", err)
	}
}

// TestLinuxPodHandshakeDisabledIsNoop proves a disabled gate performs no
// handshake and reports ok=false with no error.
func TestLinuxPodHandshakeDisabledIsNoop(t *testing.T) {
	_, ok, err := (linuxpodConfig{}).handshake(context.Background())
	if ok || err != nil {
		t.Errorf("disabled handshake = (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}

// TestLinuxPodHandshakeUnreachableFailsLoudly proves enabling the backend against
// a socket nobody serves fails with a clear, actionable error.
func TestLinuxPodHandshakeUnreachableFailsLoudly(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "absent.sock")
	_, ok, err := (linuxpodConfig{enabled: true, helperSocket: socket}).handshake(context.Background())
	if ok || err == nil {
		t.Fatalf("unreachable handshake should fail, got ok=%v err=%v", ok, err)
	}
	if !strings.Contains(err.Error(), "handshake") || !strings.Contains(err.Error(), socket) {
		t.Errorf("error %q should name the handshake and socket", err.Error())
	}
}

// TestLinuxPodHandshakeAgainstRealSocket proves the gate's client speaks the
// contract over a real unix socket: a goroutine serves FakeBackend on a temp
// socket via linuxpod.Serve, and the handshake Pings it successfully. This
// exercises the same transport production uses, end to end, without Swift.
func TestLinuxPodHandshakeAgainstRealSocket(t *testing.T) {
	// A short dir keeps the socket path under the unix sun_path limit (~104 on
	// macOS); t.TempDir() embeds the long test name and would overflow it.
	dir, err := os.MkdirTemp("", "lp")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "h.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()

	backend := linuxpod.NewFakeBackend()
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func() { _ = linuxpod.Serve(context.Background(), conn, backend) }()
		}
	}()

	info, ok, err := (linuxpodConfig{enabled: true, helperSocket: socket}).handshake(context.Background())
	if err != nil || !ok {
		t.Fatalf("handshake against real socket: ok=%v err=%v", ok, err)
	}
	if info.ProtocolVersion != linuxpod.ProtocolVersion {
		t.Errorf("protocol version = %d, want %d", info.ProtocolVersion, linuxpod.ProtocolVersion)
	}
	if !info.Simulated {
		t.Errorf("fake backend should report Simulated=true, got %+v", info)
	}
	// The handshake must surface the kubelet-surface capabilities (CRI-L4, #129) so
	// the operator log can report what the helper backs.
	if !info.Capabilities.Logs || !info.Capabilities.Exec || !info.Capabilities.Stats {
		t.Errorf("fake backend should advertise logs/exec/stats capabilities, got %+v", info.Capabilities)
	}
}
