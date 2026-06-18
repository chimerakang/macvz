package provider

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
)

// echoListener starts a loopback TCP echo server standing in for a process
// listening inside a micro-VM. It returns the listening port and a stop func.
func echoListener(t *testing.T) (int, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() { _ = ln.Close() }
}

// runningPodProvider creates a Provider with one running Pod whose micro-VM
// address is loopback, so PortForward dials a local test listener.
func runningPodProvider(t *testing.T) *Provider {
	t.Helper()
	rt := newRecordingRuntime()
	rt.runningIP = "127.0.0.1"
	p := New("mac-01", rt)
	if err := p.CreatePod(context.Background(), testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	return p
}

func TestPortForwardProxiesBytes(t *testing.T) {
	port, stop := echoListener(t)
	defer stop()
	p := runningPodProvider(t)

	client, server := net.Pipe() // server end is handed to PortForward
	pfErr := make(chan error, 1)
	go func() {
		pfErr <- p.PortForward(context.Background(), "default", "p1", int32(port), server)
	}()

	// Write through the forward and read the echoed bytes back.
	go func() { _, _ = client.Write([]byte("ping")) }()
	buf := make([]byte, 4)
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("read echoed bytes: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("echoed %q, want ping", buf)
	}

	// Closing the client side must end the forward without leaking goroutines.
	_ = client.Close()
	select {
	case err := <-pfErr:
		if err != nil {
			t.Errorf("PortForward returned %v, want nil after clean close", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("PortForward did not return after stream close (leak)")
	}
}

func TestPortForwardCancellationReturns(t *testing.T) {
	port, stop := echoListener(t)
	defer stop()
	p := runningPodProvider(t)

	ctx, cancel := context.WithCancel(context.Background())
	_, server := net.Pipe()
	pfErr := make(chan error, 1)
	go func() { pfErr <- p.PortForward(ctx, "default", "p1", int32(port), server) }()

	cancel() // tearing down the forward must unblock the proxy
	select {
	case <-pfErr:
	case <-time.After(3 * time.Second):
		t.Fatal("PortForward did not return after context cancellation (leak)")
	}
}

func TestPortForwardUnknownPod(t *testing.T) {
	p, _ := newTestProvider()
	_, server := net.Pipe()
	err := p.PortForward(context.Background(), "default", "missing", 80, server)
	if !errdefs.IsNotFound(err) {
		t.Errorf("PortForward to unknown pod = %v, want NotFound", err)
	}
}

func TestPortForwardNotRunningPod(t *testing.T) {
	p := runningPodProvider(t)
	// Flip the workload to stopped: port-forward must fail clearly, not hang.
	rtErrToStopped(p)
	_, server := net.Pipe()
	err := p.PortForward(context.Background(), "default", "p1", 8080, server)
	if err == nil {
		t.Fatal("expected error port-forwarding to a non-running pod")
	}
	if errdefs.IsNotFound(err) {
		t.Errorf("non-running pod should not be reported as NotFound: %v", err)
	}
}

func TestPortForwardRejectsBadPort(t *testing.T) {
	p := runningPodProvider(t)
	_, server := net.Pipe()
	for _, port := range []int32{0, -1, 70000} {
		if err := p.PortForward(context.Background(), "default", "p1", port, server); err == nil {
			t.Errorf("PortForward(port=%d) = nil, want range error", port)
		}
	}
}

func TestPortForwardNothingListening(t *testing.T) {
	// Reserve a port then release it, so the dial is refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	p := runningPodProvider(t)
	_, server := net.Pipe()
	if err := p.PortForward(context.Background(), "default", "p1", int32(port), server); err == nil {
		t.Fatal("expected dial error when nothing is listening on the port")
	}
}

// rtErrToStopped flips the provider's single workload to a stopped status.
func rtErrToStopped(p *Provider) {
	rt := p.rt.(*recordingRuntime)
	rt.setStatus("wl-1", runtime.Status{ID: "wl-1", Phase: runtime.PhaseStopped})
}
