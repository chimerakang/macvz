package privhelper

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveOwnerSpec(t *testing.T) {
	cases := []struct {
		spec     string
		uid, gid int
		wantErr  bool
	}{
		{"", -1, -1, false},
		{"501:20", 501, 20, false},
		{"0:0", 0, 0, false},
		{"501", -1, -1, true},
		{"x:20", -1, -1, true},
		{"501:y", -1, -1, true},
	}
	for _, c := range cases {
		uid, gid, err := ResolveOwnerSpec(c.spec)
		if (err != nil) != c.wantErr {
			t.Errorf("ResolveOwnerSpec(%q) err=%v wantErr=%t", c.spec, err, c.wantErr)
		}
		if err == nil && (uid != c.uid || gid != c.gid) {
			t.Errorf("ResolveOwnerSpec(%q) = %d:%d, want %d:%d", c.spec, uid, gid, c.uid, c.gid)
		}
	}
}

func testConfig(dir string) LaunchdConfig {
	return LaunchdConfig{
		Label:         "com.test.macvz-netd",
		PlistDir:      filepath.Join(dir, "LaunchDaemons"),
		BinaryPath:    filepath.Join(dir, "sbin", "macvz-netd"),
		SocketPath:    filepath.Join(dir, "netd.sock"),
		ConfigPath:    "/etc/macvz/config.yaml",
		OwnerUID:      501,
		OwnerGID:      20,
		StdoutPath:    filepath.Join(dir, "out.log"),
		StderrPath:    filepath.Join(dir, "err.log"),
		NewsyslogPath: filepath.Join(dir, "newsyslog.d", "macvz-netd.conf"),
	}
}

func TestRenderPlist(t *testing.T) {
	cfg := testConfig(t.TempDir())
	out, err := cfg.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		"<string>com.test.macvz-netd</string>",
		"<string>serve</string>",
		"<string>--socket</string>",
		"<string>" + cfg.SocketPath + "</string>",
		"<string>--config</string>",
		"<string>/etc/macvz/config.yaml</string>",
		"<string>--owner</string>",
		"<string>501:20</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<key>EnvironmentVariables</key>",
		"<key>PATH</key>",
		"<string>" + DefaultDaemonPath + "</string>",
		"<string>" + cfg.StdoutPath + "</string>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plist missing %q\n%s", want, out)
		}
	}
}

func TestRenderPlistOmitsOwnerWhenUnset(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.OwnerUID, cfg.OwnerGID = -1, -1
	out, err := cfg.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(out, "--owner") {
		t.Errorf("plist should omit --owner when owner unset\n%s", out)
	}
}

func TestRenderPlistConfigPath(t *testing.T) {
	cfg := testConfig(t.TempDir())

	// Emitted in production mode.
	out, err := cfg.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{"<string>--config</string>", "<string>/etc/macvz/config.yaml</string>"} {
		if !strings.Contains(out, want) {
			t.Errorf("plist missing %q\n%s", want, out)
		}
	}

	// A relative config path is rejected.
	cfg.ConfigPath = "config.yaml"
	if err := cfg.Validate(); err == nil {
		t.Error("relative ConfigPath should fail validation")
	}
}

func TestRenderPlistUnsafeNoConfigIsExplicit(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.ConfigPath = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("missing ConfigPath should fail unless explicitly unsafe")
	}
	cfg.AllowUnsafeNoConfig = true
	out, err := cfg.Render()
	if err != nil {
		t.Fatalf("Render unsafe: %v", err)
	}
	if strings.Contains(out, "--config") {
		t.Errorf("unsafe no-config plist should omit --config\n%s", out)
	}
	if !strings.Contains(out, "--allow-unsafe-no-config") {
		t.Errorf("unsafe no-config plist should include explicit flag\n%s", out)
	}
}

func TestValidate(t *testing.T) {
	good := testConfig(t.TempDir())
	if err := good.Validate(); err != nil {
		t.Fatalf("good config rejected: %v", err)
	}

	bad := good
	bad.Label = "has space"
	if err := bad.Validate(); err == nil {
		t.Error("label with space should be rejected")
	}

	rel := good
	rel.SocketPath = "relative.sock"
	if err := rel.Validate(); err == nil {
		t.Error("relative socket path should be rejected")
	}

	halfOwner := good
	halfOwner.OwnerGID = -1
	if err := halfOwner.Validate(); err == nil {
		t.Error("half-set owner should be rejected")
	}
}

// recordingRunner captures launchctl invocations and returns canned results.
type recordingRunner struct {
	calls [][]string
	err   error
	out   string
}

func (r *recordingRunner) run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return r.out, r.err
}

func TestInstallWritesPlistAndBinaryThenLoads(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	cfg.StdoutPath = filepath.Join(dir, "logs", "netd", "out.log")
	cfg.StderrPath = filepath.Join(dir, "logs", "netd", "err.log")

	// A fake source binary to install.
	src := filepath.Join(dir, "build", "macvz-netd")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	rec := &recordingRunner{}
	inst := &Installer{Cfg: cfg, run: rec.run}
	if err := inst.Install(context.Background(), src); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if _, err := os.Stat(cfg.PlistPath()); err != nil {
		t.Errorf("plist not written: %v", err)
	}
	if fi, err := os.Stat(cfg.BinaryPath); err != nil {
		t.Errorf("binary not installed: %v", err)
	} else if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("installed binary not executable: %v", fi.Mode())
	}
	if _, err := os.Stat(cfg.NewsyslogPath); err != nil {
		t.Errorf("newsyslog rotation config not written: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(cfg.StdoutPath)); err != nil {
		t.Errorf("log dir not created: %v", err)
	}

	// Install boots out any prior job, then bootstraps the new one.
	if len(rec.calls) != 2 {
		t.Fatalf("expected bootout then bootstrap, got %v", rec.calls)
	}
	if got := rec.calls[0]; got[0] != "launchctl" || got[1] != "bootout" || got[2] != cfg.ServiceTarget() {
		t.Errorf("first call = %v, want launchctl bootout %s", got, cfg.ServiceTarget())
	}
	if got := rec.calls[1]; got[0] != "launchctl" || got[1] != "bootstrap" || got[2] != "system" || got[3] != cfg.PlistPath() {
		t.Errorf("second call = %v, want launchctl bootstrap system %s", got, cfg.PlistPath())
	}
}

func TestInstallRequiresOwner(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.OwnerUID, cfg.OwnerGID = -1, -1
	inst := &Installer{Cfg: cfg, run: (&recordingRunner{}).run}
	if err := inst.Install(context.Background(), "/bin/ls"); err == nil {
		t.Error("install without owner should fail")
	}
}

func TestUninstallRemovesEverything(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)

	// Pre-create the installed artifacts.
	if err := os.MkdirAll(cfg.PlistDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.BinaryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.newsyslogDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{cfg.PlistPath(), cfg.BinaryPath, cfg.SocketPath, cfg.NewsyslogPath} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	rec := &recordingRunner{}
	inst := &Installer{Cfg: cfg, run: rec.run}
	if err := inst.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	for _, p := range []string{cfg.PlistPath(), cfg.BinaryPath, cfg.SocketPath, cfg.NewsyslogPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%q should be removed, stat err=%v", p, err)
		}
	}
	if len(rec.calls) != 1 || rec.calls[0][1] != "bootout" {
		t.Errorf("uninstall should boot out the job, got %v", rec.calls)
	}
}

func TestStatusReportsInstalledAndLoaded(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	if err := os.MkdirAll(cfg.PlistDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.PlistPath(), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := &recordingRunner{out: "state = running"}
	inst := &Installer{Cfg: cfg, run: rec.run}
	st := inst.Status(context.Background())
	if !st.PlistInstalled {
		t.Error("plist should report installed")
	}
	if st.BinaryInstalled {
		t.Error("binary should report not installed")
	}
	if !st.Loaded {
		t.Error("loaded should be true when launchctl print succeeds")
	}
}
