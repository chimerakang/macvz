package privhelper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// startControlServer starts a Server (optionally policy-enforcing) with the
// given executor on a short /tmp socket and returns a connected Client.
func startControlServer(t *testing.T, srv *Server) *Client {
	t.Helper()
	// Unix socket paths are capped at ~104 bytes on macOS; use a short /tmp dir.
	dir, err := os.MkdirTemp("/tmp", "phc")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := srv.Listen(-1, -1); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx)
	t.Cleanup(func() { cancel(); srv.Close() })
	return NewClient(srv.socketPath)
}

func tmpSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "phc")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// okExec is a no-op executor that always succeeds.
func okExec(_ context.Context, _ string, _ []string, _ string) (string, string, int, error) {
	return "", "", 0, nil
}

func TestStatusReportsHelperIdentity(t *testing.T) {
	srv := NewServerWithExec(tmpSock(t), okExec).SetVersion("v1.2.3")
	c := startControlServer(t, srv)

	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Version != "v1.2.3" {
		t.Errorf("version = %q, want v1.2.3", st.Version)
	}
	if st.Protocol != Protocol {
		t.Errorf("protocol = %d, want %d", st.Protocol, Protocol)
	}
	if st.PolicyEnforced {
		t.Error("PolicyEnforced should be false for a non-policy server")
	}
	if st.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", st.PID, os.Getpid())
	}
	if len(st.AllowedCommands) == 0 || st.Uptime == "" || st.StartedAt.IsZero() {
		t.Errorf("incomplete status: %+v", st)
	}
}

func TestStatusReportsPolicyEnforced(t *testing.T) {
	srv := NewServerWithPolicy(tmpSock(t), Policy{MeshInterface: "utun7"})
	srv.exec = okExec // don't actually run commands
	c := startControlServer(t, srv)

	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.PolicyEnforced {
		t.Error("PolicyEnforced should be true for a policy server")
	}
	// Version defaults to "dev" when unset.
	if st.Version != "dev" {
		t.Errorf("version = %q, want dev", st.Version)
	}
}

// TestUnsupportedProtocolRejected verifies a client speaking a future protocol
// is refused with a structured error rather than silently misinterpreted.
func TestUnsupportedProtocolRejected(t *testing.T) {
	srv := NewServerWithExec(tmpSock(t), okExec)
	c := startControlServer(t, srv)

	resp, err := c.roundTrip(context.Background(), Request{Protocol: Protocol + 1, Op: OpStatus})
	if err != nil {
		t.Fatalf("roundTrip: %v", err)
	}
	if resp.ErrorCode != CodeUnsupportedProtocol {
		t.Errorf("errorCode = %q, want %q (err=%q)", resp.ErrorCode, CodeUnsupportedProtocol, resp.Err)
	}
	if resp.Status != nil {
		t.Error("status must not be returned for a rejected protocol")
	}
}

// TestLegacyUnversionedRequestAccepted verifies a Protocol==0 request (an older
// client that predates version negotiation) is still served.
func TestLegacyUnversionedRequestAccepted(t *testing.T) {
	srv := NewServerWithExec(tmpSock(t), okExec)
	c := startControlServer(t, srv)

	// Protocol intentionally left 0; Op exec.
	resp, err := c.roundTrip(context.Background(), Request{Name: "pfctl", Args: []string{"-e"}})
	if err != nil {
		t.Fatalf("roundTrip: %v", err)
	}
	if resp.Err != "" {
		t.Errorf("legacy request refused: %q (%s)", resp.Err, resp.ErrorCode)
	}
}

func TestUnknownOpRejected(t *testing.T) {
	srv := NewServerWithExec(tmpSock(t), okExec)
	c := startControlServer(t, srv)

	resp, err := c.roundTrip(context.Background(), Request{Protocol: Protocol, Op: "frobnicate"})
	if err != nil {
		t.Fatalf("roundTrip: %v", err)
	}
	if resp.ErrorCode != CodeUnknownOp {
		t.Errorf("errorCode = %q, want %q", resp.ErrorCode, CodeUnknownOp)
	}
}

// TestRunReturnsStructuredErrorCodes verifies Run surfaces a refusal as an
// *APIError whose Code classifies the failure for Pod-condition mapping.
func TestRunReturnsStructuredErrorCodes(t *testing.T) {
	srv := NewServerWithExec(tmpSock(t), okExec)
	c := startControlServer(t, srv)

	_, _, _, err := c.Run(context.Background(), "rm", []string{"-rf", "/"}, "")
	if err == nil {
		t.Fatal("expected error for disallowed command")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *APIError: %T (%v)", err, err)
	}
	if apiErr.Code != CodeNotAllowed {
		t.Errorf("code = %q, want %q", apiErr.Code, CodeNotAllowed)
	}
}

// TestPolicyRefusalCode verifies an allowlisted-but-out-of-scope command is
// rejected with CodeNotPermitted (#41 integration through the #39 API).
func TestPolicyRefusalCode(t *testing.T) {
	srv := NewServerWithPolicy(tmpSock(t), Policy{MeshInterface: "utun7"})
	srv.exec = okExec
	c := startControlServer(t, srv)

	// ifconfig on a foreign interface is allowlisted but not policy-permitted.
	_, _, _, err := c.Run(context.Background(), "ifconfig", []string{"en0", "up"}, "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *APIError: %T (%v)", err, err)
	}
	if apiErr.Code != CodeNotPermitted {
		t.Errorf("code = %q, want %q", apiErr.Code, CodeNotPermitted)
	}
}

// TestMalformedRequestRejected verifies non-JSON input gets a structured
// malformed error rather than crashing the connection handler.
func TestMalformedRequestRejected(t *testing.T) {
	srv := NewServerWithExec(tmpSock(t), okExec)
	startControlServer(t, srv)

	conn, err := net.Dial("unix", srv.socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("this is not json\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	var resp Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ErrorCode != CodeMalformed {
		t.Errorf("errorCode = %q, want %q", resp.ErrorCode, CodeMalformed)
	}
}

// TestOversizedRequestRejected verifies a request beyond the size cap is refused
// rather than buffered unbounded by the root daemon.
func TestOversizedRequestRejected(t *testing.T) {
	srv := NewServerWithExec(tmpSock(t), okExec)
	startControlServer(t, srv)

	conn, err := net.Dial("unix", srv.socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// A valid JSON envelope with an enormous Stdin, exceeding maxRequestBytes.
	huge := strings.Repeat("A", maxRequestBytes+1024)
	if err := json.NewEncoder(conn).Encode(Request{Protocol: Protocol, Name: "pfctl", Stdin: huge}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var resp Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ErrorCode != CodeMalformed {
		t.Errorf("errorCode = %q, want %q (the truncated payload must not decode to a valid request)", resp.ErrorCode, CodeMalformed)
	}
}
