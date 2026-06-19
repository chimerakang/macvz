// Package container implements runtime.Runtime over Apple's open-source
// `apple/container` tool, which runs each workload as an isolated Linux
// micro-VM via Virtualization.framework. The driver shells out to the
// `container` CLI on the local host; the CLI talks to the resident
// container-apiserver service over its own socket.
//
// This is the P1 single-host driver: one Go process drives apple/container on
// one Mac. Multi-host scheduling and networking land in later phases.
package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chimerakang/macvz/internal/types"
	"github.com/chimerakang/macvz/pkg/runtime"
	"k8s.io/klog/v2"
)

// DefaultBinary is the CLI invoked when Config.Binary is empty. It is a bare
// command name resolved via PATH, never a hardcoded absolute path.
const DefaultBinary = "container"

// Target platform for micro-VM rootfs. macvz runs Linux guests on Apple
// Silicon, so images must provide a linux/arm64 variant. These name the single
// architectural assumption of the project in one place.
const (
	targetOS   = "linux"
	targetArch = "arm64"
)

// Config configures the apple/container driver. Every field is optional and
// surfaced by the caller via flags/env; see cmd/macvz-kubelet.
type Config struct {
	// Binary is the path or name of the apple/container CLI. Defaults to
	// DefaultBinary, resolved through PATH.
	Binary string
	// Rosetta enables running linux/amd64 images via Rosetta-for-Linux
	// translation on Apple Silicon. When false (the default), only linux/arm64
	// images are accepted and amd64-only images are rejected with a clear,
	// actionable error.
	Rosetta bool
}

// Driver implements runtime.Runtime (and runtime.Pinger) over apple/container.
//
// Lifecycle operations on a given workload are serialized by a per-ID lock, so
// the Driver is safe for concurrent use across many workloads while never
// racing two mutations of the same one. Operations are idempotent: creating an
// existing workload returns its ID, and stopping/destroying a missing one
// succeeds (the desired end state already holds).
type Driver struct {
	run   runner
	locks keyedMutex
	// rosetta allows booting linux/amd64 images via Rosetta translation.
	rosetta bool
}

var (
	_ runtime.Runtime = (*Driver)(nil)
	_ runtime.Pinger  = (*Driver)(nil)
)

// New returns a Driver backed by the apple/container CLI described by cfg.
func New(cfg Config) *Driver {
	bin := cfg.Binary
	if bin == "" {
		bin = DefaultBinary
	}
	return &Driver{run: &cliRunner{bin: bin}, rosetta: cfg.Rosetta}
}

// Ready reports whether the apple/container service is reachable and healthy by
// querying `container system status`. A failure is wrapped with ErrNotReady.
func (d *Driver) Ready(ctx context.Context) error {
	if _, err := d.run.output(ctx, "system", "status"); err != nil {
		return fmt.Errorf("%w: %v", runtime.ErrNotReady, err)
	}
	return nil
}

// Pull fetches an OCI image into the local content store and verifies it
// provides a variant the micro-VM can boot under the driver's architecture
// policy: linux/arm64 always, and linux/amd64 when Rosetta is enabled. An image
// with no bootable variant pulls without error but is rejected here with a
// clear ErrIncompatibleArch, rather than failing later with the runtime's
// cryptic create-time message.
//
// When auth is non-nil the pull authenticates against the registry: apple/container
// has no inline pull credentials, so the driver logs in to the registry (password
// over stdin, never argv), pulls, then logs out again so the credential does not
// linger in the runtime's credential store beyond the pull (#49). The
// login/pull/logout sequence is serialized per registry server, since the login
// is global runtime state two concurrent pulls would otherwise race.
func (d *Driver) Pull(ctx context.Context, image string, auth *runtime.RegistryAuth) error {
	if image == "" {
		return fmt.Errorf("pull: image reference is empty")
	}
	if auth != nil && auth.Server != "" {
		defer d.locks.lock("registry:" + auth.Server)()
		if err := d.registryLogin(ctx, auth); err != nil {
			return fmt.Errorf("pull %q: %w", image, err)
		}
		// Best-effort logout: the image is already in the local store, so a failed
		// logout must not fail the pull, but it is logged so a lingering credential
		// is visible. nil-context guard keeps logout running even if ctx is done.
		defer d.registryLogout(image, auth)
	}
	if _, err := d.run.output(ctx, "image", "pull", image); err != nil {
		return fmt.Errorf("pull %q: %w", image, mapErr(err))
	}
	_, err := d.selectPlatform(ctx, image)
	return err
}

// registryLogin authenticates against auth.Server using the apple/container
// `registry login` command. The password is supplied on stdin via
// `--password-stdin` so it never appears in process arguments or logs.
func (d *Driver) registryLogin(ctx context.Context, auth *runtime.RegistryAuth) error {
	args := []string{"registry", "login", "--username", auth.Username, "--password-stdin", auth.Server}
	s := streams{Stdin: strings.NewReader(auth.Password)}
	if err := d.run.run(ctx, s, args...); err != nil {
		// mapErr is applied, but the credential is never part of the args/stderr the
		// CLI echoes, so the wrapped error is safe to surface.
		return fmt.Errorf("authenticate to registry %q: %w", auth.Server, mapErr(err))
	}
	return nil
}

// registryLogout drops the credential established by registryLogin. It is
// best-effort and detached from the pull's context so cancellation of the pull
// still clears the login; failures are reported through the caller's logging.
func (d *Driver) registryLogout(image string, auth *runtime.RegistryAuth) {
	if _, err := d.run.output(context.Background(), "registry", "logout", auth.Server); err != nil {
		// Logged, not returned: the pull succeeded and the credential, if it
		// lingers, is in the runtime's own store (not a repo-controlled path).
		logPullSecretWarning(image, auth.Server, err)
	}
}

// imageInspect is the subset of `container image inspect` JSON used to confirm
// the image advertises a bootable arm64/linux variant.
type imageInspect struct {
	Variants []struct {
		Config struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
		} `json:"config"`
	} `json:"variants"`
}

// emulatedArch is the secondary guest architecture MacVz can boot on Apple
// Silicon through Rosetta-for-Linux translation, when explicitly enabled.
const emulatedArch = "amd64"

// platformChoice is the resolved platform to boot an image with, plus whether
// it requires Rosetta translation (an amd64 guest on Apple Silicon).
type platformChoice struct {
	// platform is the OCI platform string, e.g. "linux/arm64" or "linux/amd64".
	platform string
	// rosetta is true when booting this platform needs Rosetta translation.
	rosetta bool
}

// selectPlatform inspects the image and chooses the variant to boot under the
// driver's architecture policy: native linux/arm64 when available, otherwise
// linux/amd64 via Rosetta when Rosetta is enabled. It returns a clear,
// actionable ErrIncompatibleArch — tailored to whether an amd64 variant exists
// and whether Rosetta is enabled — when no bootable variant is found.
func (d *Driver) selectPlatform(ctx context.Context, image string) (platformChoice, error) {
	out, err := d.run.output(ctx, "image", "inspect", image)
	if err != nil {
		return platformChoice{}, fmt.Errorf("verify arch %q: %w", image, mapErr(err))
	}
	var results []imageInspect
	if err := json.Unmarshal(out, &results); err != nil {
		return platformChoice{}, fmt.Errorf("verify arch %q: parse inspect output: %w", image, err)
	}
	if len(results) == 0 {
		return platformChoice{}, fmt.Errorf("verify arch %q: %w", image, runtime.ErrNotFound)
	}

	seen := map[string]bool{}
	var found []string
	hasAMD64 := false
	for _, v := range results[0].Variants {
		os, arch := v.Config.OS, v.Config.Architecture
		if os == "" || arch == "" || os == "unknown" || arch == "unknown" {
			continue // skip attestation / unknown manifest entries
		}
		if os == targetOS && arch == targetArch {
			// Native arm64 is always preferred, no translation needed.
			return platformChoice{platform: targetOS + "/" + targetArch}, nil
		}
		if os == targetOS && arch == emulatedArch {
			hasAMD64 = true
		}
		if plat := os + "/" + arch; !seen[plat] {
			seen[plat] = true
			found = append(found, plat)
		}
	}

	// No native arm64. Fall back to amd64 via Rosetta when allowed.
	if hasAMD64 && d.rosetta {
		return platformChoice{platform: targetOS + "/" + emulatedArch, rosetta: true}, nil
	}
	if len(found) == 0 {
		found = append(found, "none")
	}
	if hasAMD64 && !d.rosetta {
		return platformChoice{}, fmt.Errorf("%w: image %q has no %s/%s variant (found: %s); enable Rosetta-for-Linux (runtimeRosetta: true) to run amd64 images on Apple Silicon, or use an arm64/multi-arch image",
			runtime.ErrIncompatibleArch, image, targetOS, targetArch, strings.Join(found, ", "))
	}
	return platformChoice{}, fmt.Errorf("%w: image %q has no %s/%s%s variant (found: %s); macvz boots arm64 micro-VMs on Apple Silicon",
		runtime.ErrIncompatibleArch, image, targetOS, targetArch, rosettaSuffix(d.rosetta), strings.Join(found, ", "))
}

// logPullSecretWarning reports a failed registry logout without exposing any
// credential: only the image, the registry server, and the runtime error.
func logPullSecretWarning(image, server string, err error) {
	klog.V(2).InfoS("registry logout after authenticated pull failed; credential may linger in the runtime store",
		"image", image, "registry", server, "err", err)
}

// rosettaSuffix names the amd64 fallback in error messages when Rosetta is on.
func rosettaSuffix(rosetta bool) string {
	if rosetta {
		return " or " + targetOS + "/" + emulatedArch
	}
	return ""
}

// Create provisions (but does not start) a workload, returning its ID. The
// workload ID is spec.Name, which the provider guarantees unique per host.
func (d *Driver) Create(ctx context.Context, spec types.ContainerSpec) (string, error) {
	if spec.Name == "" {
		return "", fmt.Errorf("create: spec.Name is required")
	}
	if spec.Image == "" {
		return "", fmt.Errorf("create: spec.Image is required for %q", spec.Name)
	}

	defer d.locks.lock(spec.Name)()

	args := []string{"create", "--name", spec.Name}

	// With Rosetta enabled, pin the platform explicitly: native arm64 when the
	// image has it, otherwise amd64 with translation. The runtime defaults to the
	// host arch (arm64) and will not auto-select amd64, so an amd64-only image
	// needs the platform spelled out. With Rosetta disabled we leave platform
	// unset; the runtime then rejects an arm64-less image, which mapErr surfaces
	// as ErrIncompatibleArch (Pull also pre-validates).
	if d.rosetta {
		choice, err := d.selectPlatform(ctx, spec.Image)
		if err != nil {
			return "", fmt.Errorf("create %q: %w", spec.Name, err)
		}
		args = append(args, "--platform", choice.platform)
		if choice.rosetta {
			args = append(args, "--rosetta")
		}
	}

	// Environment, sorted for deterministic argument order.
	for _, k := range sortedKeys(spec.Env) {
		args = append(args, "--env", k+"="+spec.Env[k])
	}

	// DNS: inject cluster DNS so the guest resolves Service names (#37). Order is
	// preserved (resolver precedence is significant).
	for _, ns := range spec.DNS {
		args = append(args, "--dns", ns)
	}
	for _, s := range spec.DNSSearch {
		args = append(args, "--dns-search", s)
	}
	for _, o := range spec.DNSOptions {
		args = append(args, "--dns-option", o)
	}

	// CPU request: apple/container allocates whole vCPUs. Round milli-cores up
	// to the next whole core, with at least one when a request is given.
	if spec.CPUMillis > 0 {
		cpus := (spec.CPUMillis + 999) / 1000
		args = append(args, "--cpus", strconv.FormatInt(cpus, 10))
	}
	// Memory request: the CLI accepts MiB granularity with an "M" suffix.
	if spec.MemoryBytes > 0 {
		const miB = 1024 * 1024
		mib := (spec.MemoryBytes + miB - 1) / miB
		args = append(args, "--memory", strconv.FormatInt(mib, 10)+"M")
	}

	// securityContext (#52): run as a specific user/group, a read-only root
	// filesystem, and Linux capability adjustments, all of which apple/container
	// exposes as create flags.
	if spec.User != "" {
		args = append(args, "--user", spec.User)
	}
	if spec.ReadOnlyRootFS {
		args = append(args, "--read-only")
	}
	for _, c := range spec.CapAdd {
		args = append(args, "--cap-add", c)
	}
	for _, c := range spec.CapDrop {
		args = append(args, "--cap-drop", c)
	}

	// Filesystem mounts, in spec order. A tmpfs is guest-local; a bind mount
	// shares a host path over VirtioFS using the docker-style
	// "source:target[:ro]" volume syntax.
	for _, m := range spec.Mounts {
		if m.Tmpfs {
			args = append(args, "--tmpfs", m.Target)
			continue
		}
		vol := m.Source + ":" + m.Target
		if m.ReadOnly {
			vol += ":ro"
		}
		args = append(args, "--volume", vol)
	}

	args = append(args, spec.Image)
	// Command replaces the image entrypoint; Args are appended after it. When
	// Command is empty the image's own entrypoint runs with Args.
	args = append(args, spec.Command...)
	args = append(args, spec.Args...)

	out, err := d.run.output(ctx, args...)
	if err != nil {
		// Idempotent: an existing workload of the same name satisfies the
		// request, so return its ID rather than an error.
		if errors.Is(mapErr(err), runtime.ErrAlreadyExists) {
			return spec.Name, nil
		}
		return "", fmt.Errorf("create %q: %w", spec.Name, mapErr(err))
	}
	// The CLI echoes the container ID; fall back to the requested name.
	if id := strings.TrimSpace(string(out)); id != "" {
		return id, nil
	}
	return spec.Name, nil
}

// Start boots the workload's micro-VM. Starting an already-running workload is
// a no-op success.
func (d *Driver) Start(ctx context.Context, id string) error {
	defer d.locks.lock(id)()
	if _, err := d.run.output(ctx, "start", id); err != nil {
		return fmt.Errorf("start %q: %w", id, mapErr(err))
	}
	return nil
}

// Stop requests graceful shutdown, forcing after timeout. Stopping a missing or
// already-stopped workload is a no-op success.
func (d *Driver) Stop(ctx context.Context, id string, timeout time.Duration) error {
	defer d.locks.lock(id)()
	secs := int(timeout.Round(time.Second) / time.Second)
	if secs < 0 {
		secs = 0
	}
	args := []string{"stop", "--time", strconv.Itoa(secs), id}
	if _, err := d.run.output(ctx, args...); err != nil {
		if errors.Is(mapErr(err), runtime.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("stop %q: %w", id, mapErr(err))
	}
	return nil
}

// Destroy removes the workload and reclaims its resources. Destroying a missing
// workload is a no-op success.
func (d *Driver) Destroy(ctx context.Context, id string) error {
	defer d.locks.lock(id)()
	if _, err := d.run.output(ctx, "delete", "--force", id); err != nil {
		if errors.Is(mapErr(err), runtime.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("destroy %q: %w", id, mapErr(err))
	}
	return nil
}

// Status returns the current observed state of the workload.
func (d *Driver) Status(ctx context.Context, id string) (runtime.Status, error) {
	out, err := d.run.output(ctx, "inspect", id)
	if err != nil {
		return runtime.Status{}, fmt.Errorf("status %q: %w", id, mapErr(err))
	}
	return parseStatus(id, out)
}

// Logs returns a reader over the workload's output. The workload's stdout and
// stderr arrive multiplexed into the single stream (matching `kubectl logs`).
// With opts.Follow the stream stays open until the caller closes it or ctx is
// cancelled; reads exert natural backpressure on the producer via the OS pipe.
// The caller must Close the reader, which reaps the underlying process and, if
// the command failed on its own (e.g. unknown workload), returns that error.
func (d *Driver) Logs(ctx context.Context, id string, opts runtime.LogOptions) (io.ReadCloser, error) {
	args := []string{"logs"}
	if opts.Follow {
		args = append(args, "--follow")
	}
	if opts.Tail > 0 {
		args = append(args, "-n", strconv.Itoa(opts.Tail))
	}
	args = append(args, id)
	rc, err := d.run.pipe(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("logs %q: %w", id, mapErr(err))
	}
	return rc, nil
}

// Exec runs a command inside the workload, wiring the given streams, and blocks
// until it exits or ctx is cancelled.
//
// Exit-status handling distinguishes two failures: if the command could not be
// started (the workload is missing or not running) it returns ErrNotFound or
// ErrNotRunning; if the command ran but exited non-zero it returns an
// *runtime.ExitError carrying the code. A clean (zero) exit returns nil.
//
// When sio.TTY is set the CLI allocates a pseudo-terminal in the guest; the
// caller is responsible for supplying terminal-backed streams and raw mode.
func (d *Driver) Exec(ctx context.Context, id string, cmd []string, sio runtime.ExecIO) error {
	if len(cmd) == 0 {
		return fmt.Errorf("exec %q: command is empty", id)
	}
	args := []string{"exec"}
	if sio.TTY {
		args = append(args, "--tty")
	}
	if sio.Stdin != nil {
		args = append(args, "--interactive")
	}
	args = append(args, id)
	args = append(args, cmd...)

	s := streams{Stdin: sio.Stdin, Stdout: sio.Stdout, Stderr: sio.Stderr}
	err := d.run.run(ctx, s, args...)
	if err == nil {
		return nil
	}

	// mapErr surfaces start failures (not found / not running) as typed
	// sentinels and unwraps the CommandError out of the chain. If it remains a
	// CommandError with a real exit code, the command itself exited non-zero.
	mapped := mapErr(err)
	var ce *CommandError
	if errors.As(mapped, &ce) && ce.ExitCode >= 0 {
		return fmt.Errorf("exec %q: %w", id, &runtime.ExitError{Code: ce.ExitCode})
	}
	return fmt.Errorf("exec %q: %w", id, mapped)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// inspectResult is the subset of `container inspect` JSON the driver consumes.
type inspectResult struct {
	ID     string `json:"id"`
	Status struct {
		State       string `json:"state"`
		StartedDate string `json:"startedDate"`
		ExitCode    *int   `json:"exitCode"`
		ExitStatus  *int   `json:"exitStatus"`
		Networks    []struct {
			IPv4Address string `json:"ipv4Address"`
		} `json:"networks"`
	} `json:"status"`
}

// parseStatus maps an inspect payload to a runtime.Status.
func parseStatus(id string, out []byte) (runtime.Status, error) {
	var results []inspectResult
	if err := json.Unmarshal(out, &results); err != nil {
		return runtime.Status{}, fmt.Errorf("status %q: parse inspect output: %w", id, err)
	}
	if len(results) == 0 {
		return runtime.Status{}, fmt.Errorf("status %q: %w", id, runtime.ErrNotFound)
	}
	r := results[0]

	exitCode := firstInt(r.Status.ExitCode, r.Status.ExitStatus)
	st := runtime.Status{ID: r.ID, Phase: phaseFor(r.Status.State, r.Status.StartedDate, exitCode)}
	if exitCode != nil {
		st.ExitCode = *exitCode
	}
	if r.Status.StartedDate != "" {
		if t, err := time.Parse(time.RFC3339, r.Status.StartedDate); err == nil {
			st.StartedAt = t
		}
	}
	// First non-empty IPv4 address, stripped of any CIDR suffix.
	for _, n := range r.Status.Networks {
		if n.IPv4Address != "" {
			st.IP = strings.SplitN(n.IPv4Address, "/", 2)[0]
			break
		}
	}
	return st, nil
}

// phaseFor translates an apple/container state string into a runtime.Phase. A
// freshly created, never-started workload reports "stopped" with no start time,
// which we surface as PhaseCreated to distinguish it from a stopped run.
func phaseFor(state, startedDate string, exitCode *int) runtime.Phase {
	switch strings.ToLower(state) {
	case "running":
		return runtime.PhaseRunning
	case "stopped":
		if startedDate == "" {
			return runtime.PhaseCreated
		}
		if exitCode != nil && *exitCode != 0 {
			return runtime.PhaseFailed
		}
		return runtime.PhaseStopped
	default:
		return runtime.PhaseUnknown
	}
}

func firstInt(values ...*int) *int {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

// mapErr translates a CLI failure into a typed runtime sentinel where the
// stderr text is recognizable, otherwise returns it unchanged.
func mapErr(err error) error {
	var ce *CommandError
	if !errors.As(err, &ce) {
		return err
	}
	msg := strings.ToLower(ce.Stderr)
	switch {
	case strings.Contains(msg, "not found"),
		strings.Contains(msg, "no such container"):
		return fmt.Errorf("%w: %v", runtime.ErrNotFound, err)
	case strings.Contains(msg, "not running"):
		return fmt.Errorf("%w: %v", runtime.ErrNotRunning, err)
	case strings.Contains(msg, "already exists"):
		return fmt.Errorf("%w: %v", runtime.ErrAlreadyExists, err)
	case strings.Contains(msg, "unsupported platform"),
		strings.Contains(msg, "platform "+targetOS+"/"+targetArch):
		// The runtime could not find an arm64 variant to boot.
		return fmt.Errorf("%w: %v", runtime.ErrIncompatibleArch, err)
	default:
		return err
	}
}
