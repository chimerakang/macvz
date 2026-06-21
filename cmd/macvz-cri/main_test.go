package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chimerakang/macvz/pkg/criserver"
	"github.com/chimerakang/macvz/pkg/criserver/store"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestSocketPath(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     string
		wantErr  bool
	}{
		{"canonical unix", "unix:///tmp/macvz-cri.sock", "/tmp/macvz-cri.sock", false},
		{"bare absolute path", "/run/macvz/cri.sock", "/run/macvz/cri.sock", false},
		{"empty", "", "", true},
		{"relative host form", "unix://tmp/x.sock", "", true},
		{"bare relative path", "macvz-cri.sock", "", true},
		{"unsupported scheme", "tcp://127.0.0.1:1234", "", true},
		{"unix no path", "unix://", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := socketPath(tt.endpoint)
			if (err != nil) != tt.wantErr {
				t.Fatalf("socketPath(%q) err = %v, wantErr %v", tt.endpoint, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("socketPath(%q) = %q, want %q", tt.endpoint, got, tt.want)
			}
		})
	}
}

func TestPrepareSocket(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		if err := prepareSocket(filepath.Join(t.TempDir(), "missing.sock")); err != nil {
			t.Fatalf("prepareSocket(missing): %v", err)
		}
	})

	t.Run("non socket", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "file.sock")
		if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := prepareSocket(path); err == nil {
			t.Fatal("prepareSocket(non-socket) should fail")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("non-socket path should not be removed: %v", err)
		}
	})

	t.Run("live socket", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "live.sock")
		lis, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer lis.Close()
		if err := prepareSocket(path); err == nil {
			t.Fatal("prepareSocket(live socket) should fail")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("live socket path should remain: %v", err)
		}
	})

	t.Run("stale socket", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stale.sock")
		lis, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		if err := lis.Close(); err != nil {
			t.Fatal(err)
		}
		if err := prepareSocket(path); err != nil {
			t.Fatalf("prepareSocket(stale socket): %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale socket path still exists: %v", err)
		}
	})
}

func TestSetupStreamingPublishesConcretePort(t *testing.T) {
	containers, _, err := store.NewContainerStore("")
	if err != nil {
		t.Fatalf("NewContainerStore: %v", err)
	}
	id, err := store.NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	if err := containers.Put(&store.Container{
		ID:         id,
		SandboxID:  "sandbox",
		WorkloadID: store.DeriveWorkloadID(id),
		State:      store.ContainerRunning,
	}); err != nil {
		t.Fatalf("put container: %v", err)
	}
	srv := criserver.New(criserver.Options{Containers: containers})
	stop, err := setupStreaming(srv, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("setupStreaming: %v", err)
	}
	defer stop()

	resp, err := srv.Exec(context.Background(), &runtimeapi.ExecRequest{ContainerId: id, Stdout: true})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Contains(resp.GetUrl(), ":0/") {
		t.Fatalf("Exec URL still advertises port 0: %s", resp.GetUrl())
	}
}
