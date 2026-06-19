package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chimerakang/macvz/pkg/network/wireguard"
)

// writeMeshConfig writes a minimal valid kubelet config with a mesh stanza and
// the given peers, returning its path. Each peer is (name, podCIDR, publicKey).
func writeMeshConfig(t *testing.T, enabled bool, peers [][3]string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "nodeName: mac-test\n")
	fmt.Fprintf(&b, "mesh:\n  enabled: %t\n  interface: utun7\n  privateKeyFile: %s\n  address: 10.99.0.1/32\n  listenPort: 51820\n",
		enabled, filepath.Join(t.TempDir(), "wg.key"))
	if len(peers) > 0 {
		b.WriteString("  peers:\n")
		for _, p := range peers {
			fmt.Fprintf(&b, "    - name: %s\n      publicKey: %s\n      podCIDR: %s\n", p[0], p[2], p[1])
		}
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func pubKey(t *testing.T) string {
	t.Helper()
	k, err := wireguard.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return k.PublicKey().String()
}

func TestLoadMeshPeersResolvesConfiguredPeers(t *testing.T) {
	keyA, keyB := pubKey(t), pubKey(t)
	path := writeMeshConfig(t, true, [][3]string{
		{"mac-02", "10.244.2.0/24", keyA},
		{"mac-03", "10.244.3.0/24", keyB},
	})

	peers, err := loadMeshPeers(path)
	if err != nil {
		t.Fatalf("loadMeshPeers: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("peers = %d, want 2", len(peers))
	}
	got := map[string]bool{}
	for _, p := range peers {
		got[p.Name] = true
		if len(p.AllowedIPs) == 0 {
			t.Errorf("peer %q has no AllowedIPs", p.Name)
		}
	}
	if !got["mac-02"] || !got["mac-03"] {
		t.Errorf("missing peers, got %v", got)
	}
}

func TestLoadMeshPeersRefusesDisabledMesh(t *testing.T) {
	// A reload that turns the mesh off must not silently drop every peer.
	path := writeMeshConfig(t, false, nil)
	if _, err := loadMeshPeers(path); err == nil {
		t.Fatal("expected error when reloaded config disables the mesh")
	}
}

func TestLoadMeshPeersRejectsInvalidConfig(t *testing.T) {
	// A peer with an unparseable public key is invalid and must be rejected, so a
	// bad edit keeps the running peer set rather than applying garbage.
	path := writeMeshConfig(t, true, [][3]string{{"mac-02", "10.244.2.0/24", "not-a-key"}})
	if _, err := loadMeshPeers(path); err == nil {
		t.Fatal("expected error for an invalid peer public key")
	}
}

func TestLoadMeshPeersEmptyPeerSet(t *testing.T) {
	// Removing the last peer is a valid reconcile target (the node has no peers),
	// not an error — Sync would then remove all peers and their routes.
	path := writeMeshConfig(t, true, nil)
	peers, err := loadMeshPeers(path)
	if err != nil {
		t.Fatalf("loadMeshPeers: %v", err)
	}
	if len(peers) != 0 {
		t.Errorf("peers = %d, want 0", len(peers))
	}
}
