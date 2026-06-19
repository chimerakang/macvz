package podnet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/chimerakang/macvz/pkg/network/privhelper"
)

// TestRouterRoutesThroughHelper verifies WithHelperSocket sends the Router's pf
// commands through the privileged helper rather than executing them directly.
func TestRouterRoutesThroughHelper(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	fake := func(_ context.Context, name string, args []string, stdin string) (string, string, int, error) {
		mu.Lock()
		defer mu.Unlock()
		ran = append(ran, strings.TrimSpace(name+" "+strings.Join(args, " ")))
		return "", "", 0, nil
	}

	dir, err := os.MkdirTemp("/tmp", "ph")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")
	srv := privhelper.NewServerWithExec(sock, fake)
	if err := srv.Listen(-1, -1); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx)
	defer srv.Close()

	rt := New(Config{Interface: "bridge100", EnableForwarding: true}, WithHelperSocket(sock))
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := rt.Attach(ctx, Endpoint{PodKey: "default/web", PodIP: "10.244.1.2", VMIP: "192.168.64.5"}); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var sawForward, sawAnchorLoad bool
	for _, c := range ran {
		if strings.Contains(c, "sysctl -w net.inet.ip.forwarding=1") {
			sawForward = true
		}
		if strings.Contains(c, "pfctl -a macvz/pods -f -") {
			sawAnchorLoad = true
		}
	}
	if !sawForward || !sawAnchorLoad {
		t.Errorf("expected privileged commands through helper; ran=%v", ran)
	}
}
