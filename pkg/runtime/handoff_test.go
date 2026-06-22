package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHandoffLayoutDerivation(t *testing.T) {
	m := NewHandoffManager("/run/macvz/containers")
	got, err := m.Layout("macvz-cri-abcdef0123456789abcdef01")
	if err != nil {
		t.Fatalf("Layout: %v", err)
	}
	wantContainer := "/run/macvz/containers/macvz-cri-abcdef0123456789abcdef01"
	if got.ContainerDir != wantContainer {
		t.Errorf("ContainerDir = %q, want %q", got.ContainerDir, wantContainer)
	}
	if got.RootfsDir != filepath.Join(wantContainer, "rootfs") {
		t.Errorf("RootfsDir = %q", got.RootfsDir)
	}
	if got.HandoffDir != filepath.Join(wantContainer, "handoff") {
		t.Errorf("HandoffDir = %q", got.HandoffDir)
	}
	if got.IdentityFile != filepath.Join(wantContainer, "handoff", IdentityFile) {
		t.Errorf("IdentityFile = %q", got.IdentityFile)
	}
	if got.MountPoint != HandoffMountPoint {
		t.Errorf("MountPoint = %q, want %q", got.MountPoint, HandoffMountPoint)
	}

	// The default root must be the reserved production namespace so the helper
	// and handoffmeta.go agree on one layout.
	prod, err := NewHandoffManager("").Layout("macvz-cri-abcdef0123456789abcdef01")
	if err != nil {
		t.Fatalf("Layout at default root: %v", err)
	}
	if prod.HandoffDir != HandoffContainersRoot+"/macvz-cri-abcdef0123456789abcdef01/handoff" {
		t.Errorf("default-root HandoffDir = %q", prod.HandoffDir)
	}
}

func TestNewHandoffManagerDefaultsRoot(t *testing.T) {
	got, err := NewHandoffManager("").Layout("c1")
	if err != nil {
		t.Fatalf("Layout: %v", err)
	}
	if !strings.HasPrefix(got.ContainerDir, HandoffContainersRoot+"/") {
		t.Errorf("ContainerDir = %q, want prefix %q", got.ContainerDir, HandoffContainersRoot)
	}
}

func TestHandoffInvalidIDs(t *testing.T) {
	m := NewHandoffManager(t.TempDir())
	cases := map[string]string{
		"empty":           "",
		"slash":           "a/b",
		"leading slash":   "/abs",
		"parent ref":      "..",
		"embedded parent": "a/../b",
		"dotdot prefix":   "../escape",
		"single dot":      ".",
		"backslash":       `a\b`,
		"null byte":       "a\x00b",
		"space":           "a b",
		"shell semicolon": "a;rm -rf /",
		"shell dollar":    "a$(whoami)",
		"shell pipe":      "a|b",
		"leading dot":     ".hidden",
		"glob star":       "a*",
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := m.Layout(id); !errors.Is(err, ErrInvalidHandoffID) {
				t.Errorf("Layout(%q) error = %v, want ErrInvalidHandoffID", id, err)
			}
			// Create must refuse the same IDs and stage nothing.
			if _, err := m.Create(id); !errors.Is(err, ErrInvalidHandoffID) {
				t.Errorf("Create(%q) error = %v, want ErrInvalidHandoffID", id, err)
			}
		})
	}
}

func TestHandoffValidIDsAccepted(t *testing.T) {
	m := NewHandoffManager(t.TempDir())
	for _, id := range []string{
		"macvz-cri-abcdef0123456789abcdef01",
		"c1",
		"a_b.c-d",
		"0starts-with-digit",
	} {
		if _, err := m.Layout(id); err != nil {
			t.Errorf("Layout(%q) unexpected error: %v", id, err)
		}
	}
}

func TestHandoffCreateMakesDirsWithModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	// A restrictive umask must not leave the handoff dir non-writable.
	old := syscallUmask(0o077)
	defer syscallUmask(old)

	m := NewHandoffManager(t.TempDir())
	layout, err := m.Create("macvz-cri-deadbeef")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, dir := range []string{layout.ContainerDir, layout.RootfsDir, layout.HandoffDir} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if !fi.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}
	}

	hi, err := os.Stat(layout.HandoffDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := hi.Mode().Perm(); got != handoffDirMode {
		t.Errorf("handoff dir mode = %o, want %o (umask must not strip write bits)", got, handoffDirMode)
	}
	for _, tc := range []struct {
		name string
		path string
		mode os.FileMode
	}{
		{"container", layout.ContainerDir, containerDirMode},
		{"rootfs", layout.RootfsDir, rootfsDirMode},
	} {
		fi, err := os.Stat(tc.path)
		if err != nil {
			t.Fatalf("stat %s dir: %v", tc.name, err)
		}
		if got := fi.Mode().Perm(); got != tc.mode {
			t.Errorf("%s dir mode = %o, want %o (umask must not strip mode)", tc.name, got, tc.mode)
		}
	}
}

func TestHandoffCreateIsRepeatable(t *testing.T) {
	m := NewHandoffManager(t.TempDir())
	id := "macvz-cri-repeat"

	first, err := m.Create(id)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	// Drop a file in the handoff dir to prove a second Create reuses the subtree
	// rather than wiping it.
	marker := filepath.Join(first.HandoffDir, IdentityFile)
	if err := os.WriteFile(marker, []byte("identity=x\nexpected=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	second, err := m.Create(id)
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if second.HandoffDir != first.HandoffDir {
		t.Fatalf("handoff dir changed across Create calls")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("repeat Create did not preserve existing handoff contents: %v", err)
	}
}

func TestHandoffCreateFailureDoesNotDeletePreexistingSubtree(t *testing.T) {
	m := NewHandoffManager(t.TempDir())
	id := "macvz-cri-retry-failure"

	first, err := m.Create(id)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	marker := filepath.Join(first.HandoffDir, IdentityFile)
	if err := os.WriteFile(marker, []byte("identity=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(first.RootfsDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first.RootfsDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Create(id); err == nil {
		t.Fatalf("second Create unexpectedly succeeded with rootfs path blocked by a file")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("failed retry deleted existing handoff marker: %v", err)
	}
	if _, err := os.Stat(first.ContainerDir); err != nil {
		t.Fatalf("failed retry deleted existing container subtree: %v", err)
	}
}

func TestHandoffCleanupRemovesSubtree(t *testing.T) {
	m := NewHandoffManager(t.TempDir())
	layout, err := m.Create("macvz-cri-gone")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Cleanup("macvz-cri-gone"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(layout.ContainerDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ContainerDir still present after Cleanup: stat err = %v", err)
	}
}

func TestHandoffCleanupMissingIsIdempotent(t *testing.T) {
	m := NewHandoffManager(t.TempDir())
	// Never created: cleanup of a missing path is success.
	if err := m.Cleanup("macvz-cri-never"); err != nil {
		t.Errorf("first Cleanup of missing path: %v", err)
	}
	// Repeated cleanup is also success.
	if err := m.Cleanup("macvz-cri-never"); err != nil {
		t.Errorf("repeated Cleanup: %v", err)
	}
}

func TestHandoffCleanupInvalidIDIsNoop(t *testing.T) {
	m := NewHandoffManager(t.TempDir())
	if err := m.Cleanup("../escape"); err != nil {
		t.Errorf("Cleanup of invalid id should be a no-op, got: %v", err)
	}
}
