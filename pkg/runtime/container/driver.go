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
	return &Driver{run: &cliRunner{bin: bin}}
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
// provides a linux/arm64 variant the micro-VM can boot. A non-arm64 image pulls
// without error but is rejected here with a clear ErrIncompatibleArch, rather
// than failing later with the runtime's cryptic create-time message.
func (d *Driver) Pull(ctx context.Context, image string) error {
	if image == "" {
		return fmt.Errorf("pull: image reference is empty")
	}
	if _, err := d.run.output(ctx, "image", "pull", image); err != nil {
		return fmt.Errorf("pull %q: %w", image, mapErr(err))
	}
	return d.verifyArch(ctx, image)
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

// verifyArch confirms the image exposes a linux/arm64 variant. It returns a
// clear, actionable ErrIncompatibleArch listing the architectures that were
// found when arm64 is absent.
func (d *Driver) verifyArch(ctx context.Context, image string) error {
	out, err := d.run.output(ctx, "image", "inspect", image)
	if err != nil {
		return fmt.Errorf("verify arch %q: %w", image, mapErr(err))
	}
	var results []imageInspect
	if err := json.Unmarshal(out, &results); err != nil {
		return fmt.Errorf("verify arch %q: parse inspect output: %w", image, err)
	}
	if len(results) == 0 {
		return fmt.Errorf("verify arch %q: %w", image, runtime.ErrNotFound)
	}

	seen := map[string]bool{}
	var found []string
	for _, v := range results[0].Variants {
		os, arch := v.Config.OS, v.Config.Architecture
		if os == "" || arch == "" || os == "unknown" || arch == "unknown" {
			continue // skip attestation / unknown manifest entries
		}
		if os == targetOS && arch == targetArch {
			return nil
		}
		if plat := os + "/" + arch; !seen[plat] {
			seen[plat] = true
			found = append(found, plat)
		}
	}
	if len(found) == 0 {
		found = append(found, "none")
	}
	return fmt.Errorf("%w: image %q has no %s/%s variant (found: %s); macvz boots arm64 micro-VMs on Apple Silicon, and amd64 emulation (Rosetta) is deferred to P4",
		runtime.ErrIncompatibleArch, image, targetOS, targetArch, strings.Join(found, ", "))
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

	// Environment, sorted for deterministic argument order.
	for _, k := range sortedKeys(spec.Env) {
		args = append(args, "--env", k+"="+spec.Env[k])
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

	st := runtime.Status{ID: r.ID, Phase: phaseFor(r.Status.State, r.Status.StartedDate)}
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
func phaseFor(state, startedDate string) runtime.Phase {
	switch strings.ToLower(state) {
	case "running":
		return runtime.PhaseRunning
	case "stopped":
		if startedDate == "" {
			return runtime.PhaseCreated
		}
		return runtime.PhaseStopped
	default:
		return runtime.PhaseUnknown
	}
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
	case strings.Contains(msg, "not found"):
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
