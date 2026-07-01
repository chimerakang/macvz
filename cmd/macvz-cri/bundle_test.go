package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/diagbundle"
)

// buildTestBundle runs the --support-bundle path unarchived into a temp dir and
// returns the bundle directory.
func buildTestBundle(t *testing.T, cfg supportBundleConfig) string {
	t.Helper()
	cfg.outDir = filepath.Join(t.TempDir(), "bundle")
	cfg.noArchive = true
	if err := runSupportBundle(context.Background(), cfg); err != nil {
		t.Fatalf("runSupportBundle: %v", err)
	}
	return cfg.outDir
}

func mustReadBundleFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("bundle file %s: %v", name, err)
	}
	return string(data)
}

// TestSupportBundleSanity verifies a bundle from a temp state dir contains the
// manifest, version metadata, and honest store summaries.
func TestSupportBundleSanity(t *testing.T) {
	stateDir := t.TempDir()
	sandboxes, _, err := store.New(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := strings.Repeat("ab", 32) // 64 lowercase hex chars, the store's ID shape
	sb := &store.Sandbox{ID: sandboxID, State: store.StateReady, CreatedAt: time.Now().UnixNano()}
	sb.Metadata.Namespace = "default"
	sb.Metadata.Name = "web"
	if err := sandboxes.Put(sb); err != nil {
		t.Fatal(err)
	}

	dir := buildTestBundle(t, supportBundleConfig{
		stateDir:      stateDir,
		listen:        "unix:///tmp/macvz-cri-bundle-test.sock",
		streamingAddr: "127.0.0.1:0",
	})

	manifest := mustReadBundleFile(t, dir, "manifest.txt")
	if !strings.Contains(manifest, "meta/version.txt") || !strings.Contains(manifest, "state/sandboxes.txt") {
		t.Errorf("manifest missing expected entries:\n%s", manifest)
	}
	version := mustReadBundleFile(t, dir, "meta/version.txt")
	if !strings.Contains(version, "macvz-cri") || !strings.Contains(version, "os/arch:") {
		t.Errorf("meta/version.txt missing version/os-arch:\n%s", version)
	}
	sandboxesTxt := mustReadBundleFile(t, dir, "state/sandboxes.txt")
	if !strings.Contains(sandboxesTxt, sandboxID) || !strings.Contains(sandboxesTxt, "default/web") {
		t.Errorf("state/sandboxes.txt missing sandbox summary:\n%s", sandboxesTxt)
	}
	containersTxt := mustReadBundleFile(t, dir, "state/containers.txt")
	if !strings.Contains(containersTxt, "(no container records)") {
		t.Errorf("state/containers.txt should report no records:\n%s", containersTxt)
	}
	socketsTxt := mustReadBundleFile(t, dir, "net/sockets.txt")
	if !strings.Contains(socketsTxt, "/tmp/macvz-cri-bundle-test.sock") {
		t.Errorf("net/sockets.txt missing CRI socket line:\n%s", socketsTxt)
	}
}

// TestSupportBundleMissingHelperSocket verifies the bundle is fail-soft: an
// unreachable LinuxPod helper socket records .error sidecars but the bundle is
// still produced and the command still succeeds.
func TestSupportBundleMissingHelperSocket(t *testing.T) {
	dir := buildTestBundle(t, supportBundleConfig{
		stateDir: t.TempDir(),
		listen:   defaultListen,
		lc:       linuxpodConfig{helperSocket: filepath.Join(t.TempDir(), "missing-helper.sock")},
	})

	if _, err := os.Stat(filepath.Join(dir, "linuxpod", "helper-info.json.error")); err != nil {
		t.Errorf("expected linuxpod/helper-info.json.error sidecar: %v", err)
	}
	manifest := mustReadBundleFile(t, dir, "manifest.txt")
	if !strings.Contains(manifest, "linuxpod/helper-info.json") || !strings.Contains(manifest, "ERROR") {
		t.Errorf("manifest should record the helper-info failure:\n%s", manifest)
	}
	// The bundle itself must still be complete.
	if _, err := os.Stat(filepath.Join(dir, "meta", "version.txt")); err != nil {
		t.Errorf("bundle should still contain meta/version.txt: %v", err)
	}
}

// TestSupportBundleRedactsJournal feeds a fake helper journal carrying secrets
// through the collection path and verifies every recognised secret is replaced
// before it reaches the bundle.
func TestSupportBundleRedactsJournal(t *testing.T) {
	workDir := t.TempDir()
	// The redactor replaces a sensitive key's value to end-of-line (over-redacting
	// trailing inline fields by design), so the fixture keeps the secrets on their
	// own lines the way the helper's indented journal does.
	journal := "{\n  \"pods\": [{\n    \"id\": \"sup-1\",\n    \"note\": \"kept\",\n    \"token\": \"abc-super-secret\",\n    \"apiKey\": \"sk-12345\"\n  }]\n}\n"
	if err := os.WriteFile(filepath.Join(workDir, "supervisor-journal.json"), []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workDir, "sup-1"), 0o700); err != nil {
		t.Fatal(err)
	}

	dir := buildTestBundle(t, supportBundleConfig{
		stateDir:      t.TempDir(),
		listen:        defaultListen,
		helperWorkDir: workDir,
	})

	got := mustReadBundleFile(t, dir, "linuxpod/supervisor-journal.json")
	if strings.Contains(got, "abc-super-secret") || strings.Contains(got, "sk-12345") {
		t.Errorf("journal secrets leaked into the bundle:\n%s", got)
	}
	if !strings.Contains(got, diagbundle.Placeholder) {
		t.Errorf("journal secrets should be replaced with %s:\n%s", diagbundle.Placeholder, got)
	}
	if !strings.Contains(got, "kept") {
		t.Errorf("non-secret journal content should survive redaction:\n%s", got)
	}
	// The residue listing exposes leftover per-pod sup-* directories.
	listing := mustReadBundleFile(t, dir, "linuxpod/helper-workdir.txt")
	if !strings.Contains(listing, "sup-1/") {
		t.Errorf("helper-workdir.txt should list sup-* residue:\n%s", listing)
	}
	// The adoption journal is absent in this fixture: fail-soft sidecar expected.
	if _, err := os.Stat(filepath.Join(dir, "linuxpod", "adoption-journal.json.error")); err != nil {
		t.Errorf("expected linuxpod/adoption-journal.json.error sidecar: %v", err)
	}
}

// TestSupportBundleLogTail verifies --bundle-log-file keeps only the tail of a
// large log and marks the truncation.
func TestSupportBundleLogTail(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "macvz-cri.log")
	big := strings.Repeat("head line that must be dropped\n", 20000) // ~620KB > 500KB cap
	tailMark := "tail line that must survive\n"
	if err := os.WriteFile(logPath, []byte(big+tailMark), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := buildTestBundle(t, supportBundleConfig{
		stateDir: t.TempDir(),
		listen:   defaultListen,
		logFiles: []string{logPath},
	})

	got := mustReadBundleFile(t, dir, "logs/macvz-cri.log")
	if int64(len(got)) > bundleLogTailBytes+1024 {
		t.Errorf("log tail too large: %d bytes", len(got))
	}
	if !strings.Contains(got, "[truncated:") {
		t.Errorf("truncated log should carry a truncation header:\n%.200s", got)
	}
	if !strings.Contains(got, tailMark) {
		t.Error("log tail should keep the last lines")
	}
}
