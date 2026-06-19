package privhelper

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Client talks to the root helper over its unix socket. It is safe for
// concurrent use: each Run opens its own short-lived connection.
type Client struct {
	socketPath string
	dialer     net.Dialer
}

// NewClient builds a client for the helper at socketPath.
func NewClient(socketPath string) *Client {
	return &Client{socketPath: socketPath}
}

// SocketPath returns the configured socket path.
func (c *Client) SocketPath() string { return c.socketPath }

// APIError is a structured refusal or transport-level failure from the helper.
// Its Code is one of the privhelper Code* constants, so a caller can map a
// failure to a Pod condition or diagnostic without parsing the message.
type APIError struct {
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return "privhelper: " + e.Message
	}
	return fmt.Sprintf("privhelper: %s (%s)", e.Message, e.Code)
}

// Ping verifies the helper is reachable by running a harmless allowlisted
// command (`sysctl -n kern.ostype`). It returns an error if the socket cannot be
// reached or the helper refuses, so startup can fail fast with a clear message.
func (c *Client) Ping(ctx context.Context) error {
	_, _, code, err := c.Run(ctx, "sysctl", []string{"-n", "kern.ostype"}, "")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("privhelper ping returned exit %d", code)
	}
	return nil
}

// Status queries the helper's self-report (version, protocol, allowlist, policy
// state, uptime) for startup logs and diagnostics. It also exercises protocol
// negotiation: an incompatible helper rejects the request with an *APIError
// whose Code is CodeUnsupportedProtocol.
func (c *Client) Status(ctx context.Context) (*HelperStatus, error) {
	resp, err := c.roundTrip(ctx, Request{Protocol: Protocol, Op: OpStatus})
	if err != nil {
		return nil, err
	}
	if resp.Err != "" {
		return nil, &APIError{Code: resp.ErrorCode, Message: resp.Err}
	}
	if resp.Status == nil {
		return nil, &APIError{Message: "helper returned no status"}
	}
	return resp.Status, nil
}

// ReloadPolicy asks a reloadable helper to refresh its config-derived policy.
// Helpers without a loader treat it as a no-op, so kubelet can call this before
// mesh peer reconciliation without needing to know how netd was started.
func (c *Client) ReloadPolicy(ctx context.Context) error {
	resp, err := c.roundTrip(ctx, Request{Protocol: Protocol, Op: OpReloadPolicy})
	if err != nil {
		return err
	}
	if resp.Err != "" {
		return &APIError{Code: resp.ErrorCode, Message: resp.Err}
	}
	return nil
}

// Run sends one command to the helper and returns its stdout, stderr, exit code,
// and any transport error. A non-zero exitCode with a nil error is a normal
// command failure (mirrors os/exec). A refusal or spawn failure is returned as
// an *APIError carrying a structured Code.
func (c *Client) Run(ctx context.Context, name string, args []string, stdin string) (stdout, stderr string, exitCode int, err error) {
	resp, err := c.roundTrip(ctx, Request{Protocol: Protocol, Op: OpExec, Name: name, Args: args, Stdin: stdin})
	if err != nil {
		return "", "", -1, err
	}
	if resp.Err != "" {
		return resp.Stdout, resp.Stderr, resp.ExitCode, &APIError{Code: resp.ErrorCode, Message: resp.Err}
	}
	return resp.Stdout, resp.Stderr, resp.ExitCode, nil
}

// roundTrip opens a short-lived connection, sends req, and decodes one Response.
func (c *Client) roundTrip(ctx context.Context, req Request) (Response, error) {
	conn, err := c.dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return Response{}, fmt.Errorf("privhelper dial %q: %w", c.socketPath, err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	}

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, fmt.Errorf("privhelper encode request: %w", err)
	}
	var resp Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf("privhelper decode response: %w", err)
	}
	return resp, nil
}
