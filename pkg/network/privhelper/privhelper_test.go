package privhelper

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// startTestServer starts a Server with a fake executor on a temp socket and
// returns a connected Client plus the recorded requests.
func startTestServer(t *testing.T, fake ExecFunc) *Client {
	t.Helper()
	// Unix socket paths are capped at ~104 bytes on macOS, so use a short /tmp
	// dir rather than the long t.TempDir() path.
	dir, err := os.MkdirTemp("/tmp", "ph")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	srv := NewServer(sock).withExec(fake)
	if err := srv.Listen(-1, -1); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); _ = srv.Close() })
	return NewClient(sock)
}

func TestRoundTripAllowedCommand(t *testing.T) {
	var mu sync.Mutex
	var gotName, gotStdin string
	var gotArgs []string
	fake := func(_ context.Context, name string, args []string, stdin string) (string, string, int, error) {
		mu.Lock()
		defer mu.Unlock()
		gotName, gotArgs, gotStdin = name, args, stdin
		return "OK-OUT", "", 0, nil
	}
	c := startTestServer(t, fake)

	out, stderr, code, err := c.Run(context.Background(), "pfctl", []string{"-a", "macvz/pods", "-f", "-"}, "rdr rules")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "OK-OUT" || stderr != "" || code != 0 {
		t.Errorf("unexpected result out=%q stderr=%q code=%d", out, stderr, code)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotName != "pfctl" || gotStdin != "rdr rules" || len(gotArgs) != 4 {
		t.Errorf("server got name=%q args=%v stdin=%q", gotName, gotArgs, gotStdin)
	}
}

func TestRefusesDisallowedCommand(t *testing.T) {
	called := false
	fake := func(_ context.Context, _ string, _ []string, _ string) (string, string, int, error) {
		called = true
		return "", "", 0, nil
	}
	c := startTestServer(t, fake)

	_, _, code, err := c.Run(context.Background(), "rm", []string{"-rf", "/"}, "")
	if err == nil {
		t.Fatal("expected error for disallowed command")
	}
	if called {
		t.Error("executor must not run a disallowed command")
	}
	if code != -1 {
		t.Errorf("code = %d, want -1", code)
	}
}

func TestNonZeroExitIsNotTransportError(t *testing.T) {
	fake := func(_ context.Context, _ string, _ []string, _ string) (string, string, int, error) {
		return "", "boom", 7, nil
	}
	c := startTestServer(t, fake)

	out, stderr, code, err := c.Run(context.Background(), "wg", []string{"show"}, "")
	if err != nil {
		t.Fatalf("non-zero exit should not be a transport error: %v", err)
	}
	if code != 7 || stderr != "boom" || out != "" {
		t.Errorf("got out=%q stderr=%q code=%d", out, stderr, code)
	}
}

func TestPing(t *testing.T) {
	fake := func(_ context.Context, name string, _ []string, _ string) (string, string, int, error) {
		if name != "sysctl" {
			return "", "", 1, fmt.Errorf("unexpected %q", name)
		}
		return "Darwin\n", "", 0, nil
	}
	c := startTestServer(t, fake)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestDialErrorOnMissingSocket(t *testing.T) {
	c := NewClient(filepath.Join(t.TempDir(), "nope.sock"))
	if _, _, _, err := c.Run(context.Background(), "pfctl", nil, ""); err == nil {
		t.Fatal("expected dial error on missing socket")
	}
}

func TestIsAllowed(t *testing.T) {
	for _, ok := range []string{"pfctl", "sysctl", "route", "ifconfig", "wg", "wireguard-go", "pkill"} {
		if !IsAllowed(ok) {
			t.Errorf("%q should be allowed", ok)
		}
	}
	for _, no := range []string{"rm", "sh", "bash", "cat", "pfctl;rm", ""} {
		if IsAllowed(no) {
			t.Errorf("%q should NOT be allowed", no)
		}
	}
}
