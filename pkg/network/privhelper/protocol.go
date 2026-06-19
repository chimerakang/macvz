// Package privhelper is the privileged network helper for MacVz (#38).
//
// apple/container is a per-user service and refuses to run as root, so the
// macvz-kubelet process must run as the operator's user. But the cross-host data
// plane needs privileged tools — pfctl, sysctl, route, ifconfig, wg,
// wireguard-go, pkill — which need root. This package bridges the two: a small daemon
// (cmd/macvz-netd) runs as root and executes a fixed allowlist of network
// commands on behalf of the user-run kubelet, which talks to it over a unix
// socket. The kubelet never needs sudo, and root is confined to the allowlisted
// commands rather than the whole process.
package privhelper

import (
	"sort"
	"time"
)

// Protocol is the wire-format version of the kubelet<->helper control API (#39).
// The client stamps it on every Request; the server refuses a Request whose
// Protocol is set and does not match, so a kubelet and helper from incompatible
// builds fail fast with a clear diagnostic instead of misinterpreting bytes. A
// Request with Protocol == 0 (an older, unversioned client) is accepted as the
// current version for backward compatibility.
const Protocol = 1

// Operation kinds carried in Request.Op. The zero value ("") is OpExec so an
// unset Op keeps the original command-execution behaviour.
const (
	// OpExec runs the allowlisted command named in Name (the default).
	OpExec = ""
	// OpStatus asks the helper to report its identity and health (no command is
	// run); the reply carries Response.Status. Used by startup diagnostics.
	OpStatus = "status"
	// OpReloadPolicy asks the helper to reload its config-derived policy, when a
	// policy loader was configured. It is idempotent and runs no host command.
	OpReloadPolicy = "reloadPolicy"
)

// Structured error codes returned in Response.ErrorCode so callers can map a
// refusal to a Pod condition or diagnostic without parsing the human message.
const (
	// CodeUnsupportedProtocol: the Request's Protocol is not understood.
	CodeUnsupportedProtocol = "unsupported_protocol"
	// CodeMalformed: the Request could not be decoded or exceeded the size limit.
	CodeMalformed = "malformed"
	// CodeNotAllowed: the command name is not on the helper's allowlist.
	CodeNotAllowed = "not_allowed"
	// CodeNotPermitted: the command is allowlisted but its arguments fall outside
	// this node's configured policy (#41).
	CodeNotPermitted = "not_permitted"
	// CodeExecError: the command could not be spawned (distinct from a non-zero
	// exit, which is reported via ExitCode with no error).
	CodeExecError = "exec_error"
	// CodeUnknownOp: the Request's Op is not a recognised operation.
	CodeUnknownOp = "unknown_op"
	// CodePolicyReloadFailed: the helper could not refresh its policy.
	CodePolicyReloadFailed = "policy_reload_failed"
)

// Request is one operation the kubelet asks the helper to perform as root.
type Request struct {
	// Protocol is the wire-format version (see Protocol). 0 means unversioned and
	// is accepted as the current version.
	Protocol int `json:"protocol,omitempty"`
	// Op selects the operation. The zero value (OpExec) runs the command in Name.
	Op string `json:"op,omitempty"`
	// Name is the allowlisted binary (e.g. "pfctl"). Never an absolute path; the
	// server resolves it against its own allowlist. Ignored for non-exec ops.
	Name string `json:"name"`
	// Args are the command arguments.
	Args []string `json:"args"`
	// Stdin, when non-empty, is fed to the command (e.g. a pf anchor ruleset or a
	// WireGuard config), avoiding temp files on disk.
	Stdin string `json:"stdin,omitempty"`
}

// Response is the helper's result for a Request.
type Response struct {
	// Protocol is the helper's wire-format version, so a client can log/compare it.
	Protocol int    `json:"protocol,omitempty"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
	// Err is a non-empty transport/spawn/refusal message (distinct from a non-zero
	// ExitCode, which is a normal command failure).
	Err string `json:"err,omitempty"`
	// ErrorCode is the machine-readable classification of Err (one of the Code*
	// constants), empty on success. Lets callers map a refusal to a Pod condition.
	ErrorCode string `json:"errorCode,omitempty"`
	// Status is set only in reply to an OpStatus request.
	Status *HelperStatus `json:"status,omitempty"`
}

// HelperStatus is the helper's self-report, returned for an OpStatus request and
// surfaced in the kubelet's startup logs and diagnostics. (Named to avoid
// colliding with the launchd install Status in launchd.go.)
type HelperStatus struct {
	// Protocol is the helper's wire-format version.
	Protocol int `json:"protocol"`
	// Version is the helper binary's build version (internal/version.Version).
	Version string `json:"version"`
	// AllowedCommands is the command allowlist the helper enforces.
	AllowedCommands []string `json:"allowedCommands"`
	// PolicyEnforced reports whether per-request argument validation (#41) is on.
	PolicyEnforced bool `json:"policyEnforced"`
	// PolicyReloadable reports whether the helper can refresh its config-derived
	// policy without restarting.
	PolicyReloadable bool `json:"policyReloadable"`
	// PID is the helper process id.
	PID int `json:"pid"`
	// StartedAt is when the helper began listening.
	StartedAt time.Time `json:"startedAt"`
	// Uptime is StartedAt relative to now, as a human-readable string.
	Uptime string `json:"uptime"`
}

// allowedCommands is the fixed set of binaries the helper will run as root. It
// is deliberately minimal: exactly the network tools podnet and wireguard shell
// out to. Anything else is refused. Keeping this an allowlist (not a denylist)
// means a compromised kubelet cannot run arbitrary commands as root.
var allowedCommands = map[string]bool{
	"pfctl":        true,
	"sysctl":       true,
	"route":        true,
	"ifconfig":     true,
	"wg":           true,
	"wireguard-go": true,
	"pkill":        true,
}

// IsAllowed reports whether name is an allowlisted command.
func IsAllowed(name string) bool { return allowedCommands[name] }

// AllowedCommands returns the sorted allowlist, for diagnostics/logging.
func AllowedCommands() []string {
	out := make([]string, 0, len(allowedCommands))
	for c := range allowedCommands {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
