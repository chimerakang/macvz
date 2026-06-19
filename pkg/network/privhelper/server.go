package privhelper

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// maxRequestBytes caps how large a single request may be. The largest legitimate
// request is a rendered pf anchor or WireGuard config in Stdin (kilobytes); 1 MiB
// is far above that and bounds the memory a single connection can force the
// root daemon to buffer.
const maxRequestBytes = 1 << 20

// ExecFunc runs an allowlisted command and returns stdout, stderr, exit code,
// and a spawn error (nil when the process ran, even if it exited non-zero). It
// is exported so other packages' tests can supply a fake executor without
// touching the host.
type ExecFunc func(ctx context.Context, name string, args []string, stdin string) (stdout, stderr string, exitCode int, err error)

// PolicyLoader refreshes the per-request validation policy from the helper's
// trusted local configuration.
type PolicyLoader func() (Policy, error)

// NewServerWithExec builds a Server with a custom command executor. The default
// (NewServer) runs commands for real; this is for tests and embedding.
func NewServerWithExec(socketPath string, fn ExecFunc) *Server {
	return &Server{socketPath: socketPath, exec: fn}
}

// Server is the root-side helper. It listens on a unix socket and runs
// allowlisted network commands on behalf of the user-run kubelet.
type Server struct {
	socketPath string
	exec       ExecFunc
	ln         net.Listener

	// policy, when set, restricts each request's arguments to this node's
	// configured interfaces, CIDRs, peers, and pf anchor (#41). nil disables
	// argument validation (name-allowlist only) — used by in-process callers and
	// tests that already construct well-formed commands.
	policyMu     sync.RWMutex
	policy       *Policy
	policyLoader PolicyLoader

	// version is the helper build version reported by an OpStatus request; set
	// via SetVersion. Empty is reported as "dev".
	version string
	// startedAt is when the socket began listening; recorded in Listen and used
	// to report uptime in an OpStatus reply.
	startedAt time.Time
}

// SetVersion records the helper build version reported in OpStatus replies. Call
// it before Serve. Returns the server for chaining.
func (s *Server) SetVersion(v string) *Server { s.version = v; return s }

// NewServer builds a helper server bound to socketPath. The socket is created in
// Listen.
func NewServer(socketPath string) *Server {
	return &Server{socketPath: socketPath, exec: realExec}
}

// NewServerWithPolicy builds a helper server that, in addition to the command
// allowlist, validates every request's arguments against policy (#41). This is
// the production constructor: a request that names an interface, route, anchor,
// or peer outside the node's config is refused before any command runs.
func NewServerWithPolicy(socketPath string, policy Policy) *Server {
	return &Server{socketPath: socketPath, exec: realExec, policy: &policy}
}

// NewServerWithPolicyLoader builds a helper server whose policy can be reloaded
// from config without restarting the daemon. The initial policy is loaded during
// construction so startup fails loud when the config is invalid.
func NewServerWithPolicyLoader(socketPath string, loader PolicyLoader) (*Server, error) {
	policy, err := loader()
	if err != nil {
		return nil, err
	}
	return &Server{socketPath: socketPath, exec: realExec, policy: &policy, policyLoader: loader}, nil
}

// withExec overrides the command executor (tests).
func (s *Server) withExec(f ExecFunc) *Server { s.exec = f; return s }

// Listen creates the unix socket. It removes any stale socket first, and (when
// ownerUID >= 0) chowns the socket to that user so the non-root kubelet can
// connect while the socket stays off-limits to everyone else (0660).
func (s *Server) Listen(ownerUID, ownerGID int) error {
	if err := os.RemoveAll(s.socketPath); err != nil {
		return fmt.Errorf("remove stale socket %q: %w", s.socketPath, err)
	}
	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", s.socketPath, err)
	}
	if err := os.Chmod(s.socketPath, 0o660); err != nil {
		ln.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}
	if s.startedAt.IsZero() {
		s.startedAt = time.Now()
	}
	if ownerUID >= 0 {
		if err := os.Chown(s.socketPath, ownerUID, ownerGID); err != nil {
			ln.Close()
			return fmt.Errorf("chown socket to %d:%d: %w", ownerUID, ownerGID, err)
		}
	}
	s.ln = ln
	return nil
}

// Serve accepts connections until ctx is cancelled or the listener closes.
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil {
		return errors.New("privhelper: Serve before Listen")
	}
	go func() {
		<-ctx.Done()
		s.ln.Close()
	}()
	klog.InfoS("privileged network helper listening", "socket", s.socketPath, "allow", AllowedCommands())
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // clean shutdown
			}
			return fmt.Errorf("accept: %w", err)
		}
		go s.handle(ctx, conn)
	}
}

// Close stops the listener and removes the socket.
func (s *Server) Close() error {
	if s.ln != nil {
		s.ln.Close()
	}
	return os.RemoveAll(s.socketPath)
}

// handle serves one request per connection (the kubelet opens a fresh
// connection per command, mirroring how exec.Command is one-shot).
func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	// Bound the bytes a single connection can make the root daemon buffer.
	dec := json.NewDecoder(bufio.NewReader(io.LimitReader(conn, maxRequestBytes)))
	var req Request
	if err := dec.Decode(&req); err != nil {
		s.writeError(conn, CodeMalformed, "decode request: "+err.Error())
		return
	}
	// Reject a request whose protocol we do not speak before acting on it, so an
	// incompatible client fails fast rather than on a misread field. 0 is an
	// older unversioned client and is accepted as the current version.
	if req.Protocol != 0 && req.Protocol != Protocol {
		klog.InfoS("privileged helper refused unsupported protocol", "got", req.Protocol, "want", Protocol)
		s.writeError(conn, CodeUnsupportedProtocol,
			fmt.Sprintf("unsupported protocol %d (helper speaks %d)", req.Protocol, Protocol))
		return
	}

	switch req.Op {
	case OpStatus:
		s.writeResponse(conn, Response{Protocol: Protocol, Status: s.status()})
		return
	case OpReloadPolicy:
		if err := s.reloadPolicy(); err != nil {
			klog.ErrorS(err, "privileged helper failed to reload policy")
			s.writeError(conn, CodePolicyReloadFailed, "reload policy: "+err.Error())
			return
		}
		s.writeResponse(conn, Response{Protocol: Protocol})
		return
	case OpExec:
		s.handleExec(ctx, conn, req)
	default:
		klog.InfoS("privileged helper refused unknown op", "op", req.Op)
		s.writeError(conn, CodeUnknownOp, "unknown operation: "+req.Op)
	}
}

// handleExec runs an allowlisted, policy-permitted command for an OpExec request.
func (s *Server) handleExec(ctx context.Context, conn net.Conn, req Request) {
	if !IsAllowed(req.Name) {
		klog.InfoS("privileged helper refused command", "name", req.Name)
		s.writeError(conn, CodeNotAllowed, "command not allowed: "+req.Name)
		return
	}
	if policy := s.currentPolicy(); policy != nil {
		if err := policy.Validate(req); err != nil {
			// Audit every refusal: this is the signal that someone tried to drive
			// the helper out of its configured scope.
			klog.InfoS("privileged helper refused out-of-scope request",
				"name", req.Name, "args", req.Args, "reason", err.Error())
			s.writeError(conn, CodeNotPermitted, "request not permitted: "+err.Error())
			return
		}
	}
	stdout, stderr, code, err := s.exec(ctx, req.Name, req.Args, req.Stdin)
	resp := Response{Protocol: Protocol, Stdout: stdout, Stderr: stderr, ExitCode: code}
	if err != nil {
		resp.Err = err.Error()
		resp.ErrorCode = CodeExecError
	}
	// Audit privileged changes at info level; keep read-only probes (the ping) at
	// V(2) so health checks do not flood the log.
	if isReadOnlyProbe(req) {
		klog.V(2).InfoS("privileged helper ran probe", "name", req.Name, "args", req.Args, "exit", code)
	} else {
		klog.InfoS("privileged helper applied change", "name", req.Name, "args", req.Args, "exit", code)
	}
	s.writeResponse(conn, resp)
}

func (s *Server) currentPolicy() *Policy {
	s.policyMu.RLock()
	defer s.policyMu.RUnlock()
	if s.policy == nil {
		return nil
	}
	p := *s.policy
	return &p
}

func (s *Server) reloadPolicy() error {
	if s.policyLoader == nil {
		return nil
	}
	p, err := s.policyLoader()
	if err != nil {
		return err
	}
	s.policyMu.Lock()
	s.policy = &p
	s.policyMu.Unlock()
	return nil
}

// status builds the helper's self-report for an OpStatus request.
func (s *Server) status() *HelperStatus {
	version := s.version
	if version == "" {
		version = "dev"
	}
	return &HelperStatus{
		Protocol:         Protocol,
		Version:          version,
		AllowedCommands:  AllowedCommands(),
		PolicyEnforced:   s.currentPolicy() != nil,
		PolicyReloadable: s.policyLoader != nil,
		PID:              os.Getpid(),
		StartedAt:        s.startedAt,
		Uptime:           time.Since(s.startedAt).Round(time.Second).String(),
	}
}

// writeError replies with a structured refusal (ExitCode -1, code, message).
func (s *Server) writeError(conn net.Conn, code, msg string) {
	s.writeResponse(conn, Response{Protocol: Protocol, ExitCode: -1, Err: msg, ErrorCode: code})
}

func (s *Server) writeResponse(conn net.Conn, resp Response) {
	b, err := json.Marshal(resp)
	if err != nil {
		b = []byte(`{"exitCode":-1,"err":"marshal response failed","errorCode":"exec_error"}`)
	}
	conn.Write(append(b, '\n'))
}

// realExec runs the command for real (server runs as root).
func realExec(ctx context.Context, name string, args []string, stdin string) (string, string, int, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != "" {
		cmd.Stdin = bytes.NewReader([]byte(stdin))
	}
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// A non-zero exit is a normal command result, not a spawn error.
			return stdout.String(), stderr.String(), exitErr.ExitCode(), nil
		}
		return stdout.String(), stderr.String(), -1, err
	}
	return stdout.String(), stderr.String(), 0, nil
}

// ResolveOwner parses a uid:gid pair (or the SUDO_UID/SUDO_GID env that sudo
// sets), returning (-1, -1) when neither is available so the socket keeps the
// default root ownership.
func ResolveOwner(uid, gid string) (int, int) {
	ui, err1 := strconv.Atoi(uid)
	gi, err2 := strconv.Atoi(gid)
	if err1 != nil || err2 != nil {
		return -1, -1
	}
	return ui, gi
}

// ResolveOwnerSpec parses a single "uid:gid" string (the form baked into the
// launchd plist at install time, where SUDO_UID/SUDO_GID are not available at
// daemon start). An empty spec returns (-1, -1) so the socket keeps default root
// ownership. A malformed spec is an error so a misconfigured plist fails loudly
// rather than silently leaving the socket unreachable by the kubelet's user.
func ResolveOwnerSpec(spec string) (int, int, error) {
	if spec == "" {
		return -1, -1, nil
	}
	uid, gid, ok := strings.Cut(spec, ":")
	if !ok {
		return -1, -1, fmt.Errorf("owner %q: want uid:gid", spec)
	}
	ui, err := strconv.Atoi(uid)
	if err != nil {
		return -1, -1, fmt.Errorf("owner uid %q: %w", uid, err)
	}
	gi, err := strconv.Atoi(gid)
	if err != nil {
		return -1, -1, fmt.Errorf("owner gid %q: %w", gid, err)
	}
	return ui, gi, nil
}
