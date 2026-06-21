package privhelper

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

// macOS launchd integration for the privileged network helper (#40).
//
// Without this, an operator must run `sudo macvz-netd` by hand before every
// kubelet start. A LaunchDaemon makes the helper a managed system service: it is
// installed once with sudo, started at boot, restarted if it crashes, and the
// kubelet then connects to its socket with no further privilege escalation.
//
// The plist runs the helper as root (the whole point — it owns pf/route/wg), and
// passes --owner uid:gid so the helper chowns its socket to the operator's user.
// At install time there is a SUDO_UID; at daemon start (launchd) there is not, so
// the owner is captured into the plist rather than read from the environment.

const (
	// DefaultLabel is the launchd job label and the basename of the plist.
	DefaultLabel = "com.github.chimerakang.macvz-netd"
	// DefaultPlistDir is where system LaunchDaemons live on macOS.
	DefaultPlistDir = "/Library/LaunchDaemons"
	// DefaultBinaryPath is where Install copies the helper binary so the plist
	// points at a stable, root-owned location rather than a user build tree.
	DefaultBinaryPath = "/usr/local/sbin/macvz-netd"
	// DefaultStdoutPath and DefaultStderrPath are the daemon's log files.
	DefaultStdoutPath = "/var/log/macvz-netd.log"
	DefaultStderrPath = "/var/log/macvz-netd.err.log"
	// DefaultDaemonPath is the PATH used by the root LaunchDaemon. launchd's
	// default environment is sparse; include Homebrew locations so wg and
	// wireguard-go resolve on Apple Silicon Macs.
	DefaultDaemonPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
)

// LaunchdConfig describes the LaunchDaemon to install. All paths are absolute so
// the same struct drives both plist rendering and the filesystem operations,
// and tests can redirect every path into a temp dir.
type LaunchdConfig struct {
	// Label is the launchd job label (reverse-DNS) and the plist basename.
	Label string
	// PlistDir is the directory the plist is written to (system LaunchDaemons).
	PlistDir string
	// BinaryPath is where the helper binary is installed and the plist points.
	BinaryPath string
	// SocketPath is the unix socket the helper listens on.
	SocketPath string
	// ConfigPath, when set, is the MacVz config the daemon loads to restrict
	// privileged commands to its interfaces, CIDRs, peers, and pf anchor (#41).
	// Empty installs a daemon with argument validation disabled (name-allowlist
	// only).
	ConfigPath string
	// AllowUnsafeNoConfig explicitly permits installing without ConfigPath. This
	// exists for local development only; production helpers should always enforce
	// config-derived request policy.
	AllowUnsafeNoConfig bool
	// OwnerUID/OwnerGID is the user the socket is chowned to so the (non-root)
	// kubelet can connect. -1 leaves the socket root-owned.
	OwnerUID int
	OwnerGID int
	// StdoutPath/StderrPath are the daemon's log destinations.
	StdoutPath string
	StderrPath string
	// NewsyslogPath is the newsyslog drop-in that rotates the daemon's logs so a
	// long-running node does not grow them without bound (#69). Empty disables
	// rotation management (the logs then grow until an external tool trims them).
	NewsyslogPath string
	// LogRotateCount/LogRotateSizeKB tune rotation: how many compressed archives
	// to keep, and the size (KB) past which a log rotates. Zero uses the defaults.
	LogRotateCount  int
	LogRotateSizeKB int
}

// DefaultLaunchdConfig returns the standard system layout with the socket and
// owner left to the caller (owner defaults to "unset").
func DefaultLaunchdConfig(socketPath string) LaunchdConfig {
	return LaunchdConfig{
		Label:         DefaultLabel,
		PlistDir:      DefaultPlistDir,
		BinaryPath:    DefaultBinaryPath,
		SocketPath:    socketPath,
		OwnerUID:      -1,
		OwnerGID:      -1,
		StdoutPath:    DefaultStdoutPath,
		StderrPath:    DefaultStderrPath,
		NewsyslogPath: DefaultNewsyslogPath,
	}
}

// PlistPath is the full path to the LaunchDaemon plist.
func (c LaunchdConfig) PlistPath() string {
	return filepath.Join(c.PlistDir, c.Label+".plist")
}

// ServiceTarget is the launchctl service target (e.g. "system/<label>") used by
// bootout and print.
func (c LaunchdConfig) ServiceTarget() string {
	return "system/" + c.Label
}

// Validate checks the config is internally consistent before any filesystem or
// launchctl side effect, so failures are caught before half-applying.
func (c LaunchdConfig) Validate() error {
	for name, p := range map[string]string{
		"label":      c.Label,
		"plistDir":   c.PlistDir,
		"binaryPath": c.BinaryPath,
		"socketPath": c.SocketPath,
		"stdoutPath": c.StdoutPath,
		"stderrPath": c.StderrPath,
	} {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("launchd config: %s is empty", name)
		}
	}
	if strings.ContainsAny(c.Label, "/ \t") {
		return fmt.Errorf("launchd config: label %q must not contain spaces or slashes", c.Label)
	}
	for name, p := range map[string]string{
		"binaryPath": c.BinaryPath,
		"socketPath": c.SocketPath,
		"stdoutPath": c.StdoutPath,
		"stderrPath": c.StderrPath,
	} {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("launchd config: %s %q must be absolute", name, p)
		}
	}
	if c.ConfigPath != "" && !filepath.IsAbs(c.ConfigPath) {
		return fmt.Errorf("launchd config: configPath %q must be absolute", c.ConfigPath)
	}
	if c.NewsyslogPath != "" && !filepath.IsAbs(c.NewsyslogPath) {
		return fmt.Errorf("launchd config: newsyslogPath %q must be absolute", c.NewsyslogPath)
	}
	if c.ConfigPath == "" && !c.AllowUnsafeNoConfig {
		return fmt.Errorf("launchd config: configPath is required unless allowUnsafeNoConfig is set")
	}
	if (c.OwnerUID < 0) != (c.OwnerGID < 0) {
		return fmt.Errorf("launchd config: owner uid/gid must both be set or both unset")
	}
	return nil
}

// ownerSpec renders "uid:gid" for the --owner flag, or "" when unset.
func (c LaunchdConfig) ownerSpec() string {
	if c.OwnerUID < 0 || c.OwnerGID < 0 {
		return ""
	}
	return fmt.Sprintf("%d:%d", c.OwnerUID, c.OwnerGID)
}

var plistTemplate = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.BinaryPath}}</string>
		<string>serve</string>
		<string>--socket</string>
		<string>{{.SocketPath}}</string>
{{- if .ConfigPath}}
		<string>--config</string>
		<string>{{.ConfigPath}}</string>
{{- end}}
{{- if .AllowUnsafeNoConfig}}
		<string>--allow-unsafe-no-config</string>
{{- end}}
{{- if .Owner}}
		<string>--owner</string>
		<string>{{.Owner}}</string>
{{- end}}
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>{{.Path}}</string>
	</dict>
	<key>StandardOutPath</key>
	<string>{{.StdoutPath}}</string>
	<key>StandardErrorPath</key>
	<string>{{.StderrPath}}</string>
</dict>
</plist>
`))

// Render produces the plist XML for this config. It is pure (no side effects)
// so it can be unit tested and previewed without root.
func (c LaunchdConfig) Render() (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	err := plistTemplate.Execute(&buf, struct {
		Label, BinaryPath, SocketPath, ConfigPath, Owner, Path, StdoutPath, StderrPath string
		AllowUnsafeNoConfig                                                            bool
	}{
		Label:               c.Label,
		BinaryPath:          c.BinaryPath,
		SocketPath:          c.SocketPath,
		ConfigPath:          c.ConfigPath,
		AllowUnsafeNoConfig: c.AllowUnsafeNoConfig,
		Owner:               c.ownerSpec(),
		Path:                DefaultDaemonPath,
		StdoutPath:          c.StdoutPath,
		StderrPath:          c.StderrPath,
	})
	if err != nil {
		return "", fmt.Errorf("render plist: %w", err)
	}
	return buf.String(), nil
}

// commandRunner runs an external command (launchctl) and returns combined
// output. It is a field on Installer so tests can record invocations without
// touching the host's launchd.
type commandRunner func(ctx context.Context, name string, args ...string) (string, error)

func realRunner(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Installer applies a LaunchdConfig to the host: copies the binary, writes the
// plist, and drives launchctl. Construct with NewInstaller.
type Installer struct {
	Cfg LaunchdConfig
	run commandRunner
}

// NewInstaller builds an Installer that drives the real launchctl.
func NewInstaller(cfg LaunchdConfig) *Installer {
	return &Installer{Cfg: cfg, run: realRunner}
}

// Status is a snapshot of the daemon's install/run state.
type Status struct {
	// PlistInstalled is true when the LaunchDaemon plist exists on disk.
	PlistInstalled bool
	// BinaryInstalled is true when the helper binary exists at BinaryPath.
	BinaryInstalled bool
	// SocketPresent is true when the listening socket exists.
	SocketPresent bool
	// Loaded is true when launchctl reports the job as bootstrapped.
	Loaded bool
	// Detail is the raw `launchctl print` output (or the reason it was absent).
	Detail string
}

// Install copies srcBinary to the configured BinaryPath, writes the plist, and
// bootstraps the job so it starts now and at every boot. It is idempotent: an
// already-loaded job is booted out first so the new plist takes effect.
func (i *Installer) Install(ctx context.Context, srcBinary string) error {
	if err := i.Cfg.Validate(); err != nil {
		return err
	}
	if i.Cfg.ownerSpec() == "" {
		return fmt.Errorf("install: owner uid/gid required so the kubelet's user can reach the socket")
	}
	if err := copyExecutable(srcBinary, i.Cfg.BinaryPath); err != nil {
		return err
	}
	plist, err := i.Cfg.Render()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(i.Cfg.PlistDir, 0o755); err != nil {
		return fmt.Errorf("create plist dir: %w", err)
	}
	if err := os.WriteFile(i.Cfg.PlistPath(), []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write plist %q: %w", i.Cfg.PlistPath(), err)
	}
	for _, p := range []string{i.Cfg.StdoutPath, i.Cfg.StderrPath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return fmt.Errorf("create log dir for %q: %w", p, err)
		}
	}
	// Install the newsyslog drop-in so the daemon's logs rotate from the first
	// run (#69). Skipped when rotation management is disabled (empty path).
	if i.Cfg.NewsyslogPath != "" {
		rotate, err := i.Cfg.RenderNewsyslog()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(i.Cfg.newsyslogDir(), 0o755); err != nil {
			return fmt.Errorf("create newsyslog dir: %w", err)
		}
		if err := os.WriteFile(i.Cfg.NewsyslogPath, []byte(rotate), 0o644); err != nil {
			return fmt.Errorf("write newsyslog config %q: %w", i.Cfg.NewsyslogPath, err)
		}
	}
	// Replace any prior load so an upgrade picks up the new binary/plist.
	_ = i.unload(ctx)
	return i.load(ctx)
}

// Load bootstraps the (already-installed) job.
func (i *Installer) Load(ctx context.Context) error {
	if _, err := os.Stat(i.Cfg.PlistPath()); err != nil {
		return fmt.Errorf("load: plist not installed at %q: %w", i.Cfg.PlistPath(), err)
	}
	return i.load(ctx)
}

// Unload boots the job out without removing the plist or binary.
func (i *Installer) Unload(ctx context.Context) error {
	return i.unload(ctx)
}

// Uninstall boots the job out and removes the plist, binary, and socket, leaving
// no running daemon and nothing on disk.
func (i *Installer) Uninstall(ctx context.Context) error {
	// Best-effort unload: if it was never loaded, bootout errors are expected.
	_ = i.unload(ctx)
	var errs []string
	if err := os.Remove(i.Cfg.PlistPath()); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Sprintf("remove plist: %v", err))
	}
	if err := os.Remove(i.Cfg.BinaryPath); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Sprintf("remove binary: %v", err))
	}
	if err := os.Remove(i.Cfg.SocketPath); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Sprintf("remove socket: %v", err))
	}
	if i.Cfg.NewsyslogPath != "" {
		if err := os.Remove(i.Cfg.NewsyslogPath); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Sprintf("remove newsyslog config: %v", err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("uninstall: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Status reports what is installed and whether launchd has the job loaded.
func (i *Installer) Status(ctx context.Context) Status {
	st := Status{}
	if _, err := os.Stat(i.Cfg.PlistPath()); err == nil {
		st.PlistInstalled = true
	}
	if _, err := os.Stat(i.Cfg.BinaryPath); err == nil {
		st.BinaryInstalled = true
	}
	if _, err := os.Stat(i.Cfg.SocketPath); err == nil {
		st.SocketPresent = true
	}
	out, err := i.run(ctx, "launchctl", "print", i.Cfg.ServiceTarget())
	st.Detail = strings.TrimSpace(out)
	if err == nil {
		st.Loaded = true
	} else if st.Detail == "" {
		st.Detail = err.Error()
	}
	return st
}

// load uses `launchctl bootstrap system <plist>`, the modern replacement for the
// deprecated `load -w`.
func (i *Installer) load(ctx context.Context) error {
	if _, err := i.run(ctx, "launchctl", "bootstrap", "system", i.Cfg.PlistPath()); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	return nil
}

// unload uses `launchctl bootout system/<label>`, the modern replacement for the
// deprecated `unload -w`.
func (i *Installer) unload(ctx context.Context) error {
	if _, err := i.run(ctx, "launchctl", "bootout", i.Cfg.ServiceTarget()); err != nil {
		return fmt.Errorf("bootout: %w", err)
	}
	return nil
}

// copyExecutable copies src to dst (0755), creating parent dirs. src and dst may
// be the same file (re-install from the installed path), which is a no-op.
func copyExecutable(src, dst string) error {
	absSrc, err := filepath.Abs(src)
	if err != nil {
		return fmt.Errorf("resolve source %q: %w", src, err)
	}
	if absSrc == dst {
		return nil
	}
	in, err := os.Open(absSrc)
	if err != nil {
		return fmt.Errorf("open source binary %q: %w", absSrc, err)
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create bin dir: %w", err)
	}
	// Write to a temp file then rename so a crash never leaves a partial binary.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".macvz-netd-*")
	if err != nil {
		return fmt.Errorf("create temp binary: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp binary: %w", err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return fmt.Errorf("chmod temp binary: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("install binary to %q: %w", dst, err)
	}
	return nil
}
