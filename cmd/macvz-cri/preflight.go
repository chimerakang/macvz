package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chimerakang/macvz/pkg/network"
)

// preflight is the CRI-P8 operator diagnostics mode (#80). `macvz-cri --preflight`
// checks the runtime dependencies an operator must satisfy before kubelet/k3s can
// use the adapter — the apple/container CLI, a writable state dir and socket path,
// and (when configured) Pod networking and mount-policy inputs — and prints a
// clear report. It never starts the server, mutates host state, or boots a VM, so
// it is safe to run repeatedly while wiring up a node.
//
// The checks are split into a pure planner (preflightChecks) over an injectable
// probe set (preflightProbes) so the logic is unit-tested without touching the
// host, and a thin main-side runner (runPreflight) that wires the real probes and
// renders the report.

// preflightConfig is the subset of adapter configuration the preflight checks
// need. main builds it from the same flags run() consumes so the diagnostics
// reflect exactly the configuration the server would start with.
type preflightConfig struct {
	listen        string
	stateDir      string
	runtimeBinary string
	pn            podNetConfig
	mc            mountConfig
}

// checkStatus is the outcome of a single preflight check.
type checkStatus string

const (
	checkOK   checkStatus = "OK"
	checkWarn checkStatus = "WARN"
	checkFail checkStatus = "FAIL"
)

// checkResult is one line of the preflight report.
type checkResult struct {
	Name   string
	Status checkStatus
	Detail string
}

// preflightProbes are the host interactions the checks need, injected so tests can
// drive every branch deterministically.
type preflightProbes struct {
	// lookPath resolves a binary on PATH (exec.LookPath).
	lookPath func(string) (string, error)
	// stat reports a path's FileInfo (os.Stat).
	stat func(string) (os.FileInfo, error)
	// dirWritable returns nil if a writable file can be created under dir.
	dirWritable func(dir string) error
	// socketServing reports whether a CRI server is already serving on a socket.
	socketServing func(path string) bool
	// validateCIDR returns nil if the Pod CIDR is a usable IPAM range.
	validateCIDR func(cidr string) error
}

// defaultPreflightProbes wires the real host interactions.
func defaultPreflightProbes() preflightProbes {
	return preflightProbes{
		lookPath:      exec.LookPath,
		stat:          os.Stat,
		dirWritable:   dirWritable,
		socketServing: socketServing,
		validateCIDR: func(cidr string) error {
			_, err := network.NewPodIPAM(cidr)
			return err
		},
	}
}

// preflightChecks evaluates the adapter's runtime dependencies and returns one
// result per check, in a stable order. It is pure given the probes: no globals, no
// direct host access.
func preflightChecks(cfg preflightConfig, p preflightProbes) []checkResult {
	var out []checkResult

	out = append(out, checkRuntimeBinary(cfg.runtimeBinary, p))
	out = append(out, checkSocketPath(cfg.listen, p))
	out = append(out, checkStateDir(cfg.stateDir, p))
	out = append(out, checkPodNetwork(cfg.pn, p))
	out = append(out, checkMounts(cfg.mc, p)...)

	return out
}

// checkRuntimeBinary verifies the apple/container CLI the adapter drives is
// present. This is the one hard runtime dependency: without it every container
// method returns FailedPrecondition.
func checkRuntimeBinary(binary string, p preflightProbes) checkResult {
	name := binary
	if name == "" {
		name = "container"
	}
	resolved, err := p.lookPath(name)
	if err != nil {
		return checkResult{
			Name:   "apple/container CLI",
			Status: checkFail,
			Detail: fmt.Sprintf("%q not found on PATH (%v); install apple/container or pass --runtime-binary", name, err),
		}
	}
	return checkResult{Name: "apple/container CLI", Status: checkOK, Detail: resolved}
}

// checkSocketPath verifies the CRI socket can be created where requested and is
// not already owned by a live server (which a second adapter would split clients
// across).
func checkSocketPath(listen string, p preflightProbes) checkResult {
	path, err := socketPath(listen)
	if err != nil {
		return checkResult{Name: "CRI socket", Status: checkFail, Detail: err.Error()}
	}
	if p.socketServing(path) {
		return checkResult{
			Name:   "CRI socket",
			Status: checkFail,
			Detail: fmt.Sprintf("%s is already serving; another macvz-cri is running", path),
		}
	}
	dir := filepath.Dir(path)
	if err := p.dirWritable(dir); err != nil {
		return checkResult{
			Name:   "CRI socket",
			Status: checkFail,
			Detail: fmt.Sprintf("socket directory %s is not writable: %v", dir, err),
		}
	}
	return checkResult{Name: "CRI socket", Status: checkOK, Detail: path}
}

// checkStateDir verifies the restart-tolerant state directory is writable. An
// empty state dir is a WARN, not a failure: the adapter still runs, but its
// sandbox/container view is in-memory and does not survive a restart.
func checkStateDir(stateDir string, p preflightProbes) checkResult {
	if stateDir == "" {
		return checkResult{
			Name:   "state dir",
			Status: checkWarn,
			Detail: "empty: sandbox/container state is in-memory only and will not survive an adapter restart",
		}
	}
	if err := p.dirWritable(stateDir); err != nil {
		return checkResult{
			Name:   "state dir",
			Status: checkFail,
			Detail: fmt.Sprintf("%s is not writable: %v", stateDir, err),
		}
	}
	return checkResult{Name: "state dir", Status: checkOK, Detail: stateDir}
}

// checkPodNetwork validates the CRI-P5 Pod networking inputs. Pod networking is
// optional, so "off" is OK and "partially configured" is a WARN that explains the
// missing flag; a bad CIDR or missing helper socket when fully configured is a
// FAIL because the path would not start.
func checkPodNetwork(pn podNetConfig, p preflightProbes) checkResult {
	if !pn.enabled() {
		if pn.podCIDR == "" && pn.iface == "" {
			return checkResult{
				Name:   "Pod networking",
				Status: checkOK,
				Detail: "disabled: sandboxes run without a Pod IP (NetworkReady=false)",
			}
		}
		return checkResult{
			Name:   "Pod networking",
			Status: checkWarn,
			Detail: "partially configured: both --pod-cidr and --pod-network-interface are required to enable it",
		}
	}
	if err := p.validateCIDR(pn.podCIDR); err != nil {
		return checkResult{
			Name:   "Pod networking",
			Status: checkFail,
			Detail: fmt.Sprintf("--pod-cidr %q is not a usable range: %v", pn.podCIDR, err),
		}
	}
	if pn.helperSocket != "" {
		if _, err := p.stat(pn.helperSocket); err != nil {
			return checkResult{
				Name:   "Pod networking",
				Status: checkFail,
				Detail: fmt.Sprintf("--pod-network-helper-socket %s is not present: %v; start macvz-netd or run the adapter as root", pn.helperSocket, err),
			}
		}
		return checkResult{
			Name:   "Pod networking",
			Status: checkOK,
			Detail: fmt.Sprintf("enabled via helper socket %s on %s", pn.helperSocket, pn.iface),
		}
	}
	return checkResult{
		Name:   "Pod networking",
		Status: checkWarn,
		Detail: fmt.Sprintf("enabled on %s without a helper socket: pf/route operations require running the adapter as root", pn.iface),
	}
}

// checkMounts validates the CRI-P7 mount-policy inputs. The kubelet pods dir is a
// WARN when absent (kubelet may not be installed yet); hostPath allowlist entries
// must be absolute, else they silently match nothing.
func checkMounts(mc mountConfig, p preflightProbes) []checkResult {
	var out []checkResult

	podsDir := mc.kubeletPodsDir
	if podsDir == "" {
		podsDir = "/var/lib/kubelet/pods"
	}
	if _, err := p.stat(podsDir); err != nil {
		out = append(out, checkResult{
			Name:   "kubelet pods dir",
			Status: checkWarn,
			Detail: fmt.Sprintf("%s not present yet: %v; required once kubelet projects volumes", podsDir, err),
		})
	} else {
		out = append(out, checkResult{Name: "kubelet pods dir", Status: checkOK, Detail: podsDir})
	}

	if len(mc.hostPathAllowed) == 0 {
		out = append(out, checkResult{
			Name:   "hostPath allowlist",
			Status: checkOK,
			Detail: "empty: arbitrary hostPath volumes are rejected (safe macOS default)",
		})
		return out
	}
	var bad []string
	for _, prefix := range mc.hostPathAllowed {
		if !filepath.IsAbs(prefix) {
			bad = append(bad, prefix)
		}
	}
	if len(bad) > 0 {
		out = append(out, checkResult{
			Name:   "hostPath allowlist",
			Status: checkFail,
			Detail: fmt.Sprintf("non-absolute prefixes match nothing: %s", strings.Join(bad, ", ")),
		})
		return out
	}
	out = append(out, checkResult{
		Name:   "hostPath allowlist",
		Status: checkOK,
		Detail: strings.Join(mc.hostPathAllowed, ", "),
	})
	return out
}

// worstStatus reduces a report to its most severe status, ranking FAIL > WARN > OK.
func worstStatus(results []checkResult) checkStatus {
	worst := checkOK
	rank := map[checkStatus]int{checkOK: 0, checkWarn: 1, checkFail: 2}
	for _, r := range results {
		if rank[r.Status] > rank[worst] {
			worst = r.Status
		}
	}
	return worst
}

// renderPreflight writes a human-readable report to w and returns whether all hard
// dependencies passed (no FAIL). WARN does not fail the report — the adapter still
// runs, the operator is just told what is degraded.
func renderPreflight(w io.Writer, results []checkResult) bool {
	width := 0
	for _, r := range results {
		if len(r.Name) > width {
			width = len(r.Name)
		}
	}
	fmt.Fprintln(w, "macvz-cri preflight (experimental CRI feasibility adapter, docs/CRI_FEASIBILITY.md CRI-P8)")
	fmt.Fprintln(w)
	// Stable order within equal insertion is preserved; sort only to group is
	// avoided so the report reads in dependency order.
	for _, r := range results {
		fmt.Fprintf(w, "  [%-4s] %-*s  %s\n", r.Status, width, r.Name, r.Detail)
	}
	fmt.Fprintln(w)
	ok := worstStatus(results) != checkFail
	if ok {
		fmt.Fprintln(w, "preflight: OK (the adapter can serve; review any WARN items above)")
	} else {
		fmt.Fprintln(w, "preflight: FAIL (resolve the FAIL items above before starting macvz-cri)")
	}
	return ok
}

// runPreflight evaluates the real host checks, prints the report, and returns an
// error when a hard dependency is missing so main can exit non-zero.
func runPreflight(cfg preflightConfig) error {
	results := preflightChecks(cfg, defaultPreflightProbes())
	if !renderPreflight(os.Stdout, results) {
		return fmt.Errorf("preflight failed: %d hard dependency check(s) did not pass", countFailures(results))
	}
	return nil
}

func countFailures(results []checkResult) int {
	n := 0
	for _, r := range results {
		if r.Status == checkFail {
			n++
		}
	}
	return n
}

// dirWritable reports whether dir exists and a file can be created under it, by
// creating and removing a probe file. It does not create dir itself: preflight
// reports the missing directory rather than silently provisioning host state.
func dirWritable(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	probe, err := os.CreateTemp(dir, ".macvz-cri-preflight-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// socketServing reports whether a live server answers on a Unix socket path.
func socketServing(path string) bool {
	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// sortedStatuses is a tiny helper used by tests to assert the set of statuses
// without depending on report order.
func sortedStatuses(results []checkResult) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, string(r.Status))
	}
	sort.Strings(out)
	return out
}
