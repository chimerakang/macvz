package linuxpod

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// client.go implements HelperClient: a Backend that forwards each call to a
// helper over the NDJSON protocol. It uses a connection-per-call model — dial,
// send one request line, read one response line, close — which keeps framing
// trivial and avoids multiplexing concurrent calls over one stream. For a unix
// socket helper that cost is negligible at CRI call rates and it makes the client
// safe for concurrent use without a write lock.

// Dialer opens a fresh connection to the helper. It is injectable so tests drive
// the client over an in-memory pipe and production dials a unix socket.
type Dialer func(ctx context.Context) (net.Conn, error)

// HelperClient is a Backend backed by a remote helper reachable through a Dialer.
type HelperClient struct {
	dial Dialer
}

// NewHelperClient returns a client that reaches the helper via dial.
func NewHelperClient(dial Dialer) *HelperClient {
	return &HelperClient{dial: dial}
}

// NewSocketClient returns a client that dials the helper's unix socket at path.
func NewSocketClient(socketPath string) *HelperClient {
	return NewHelperClient(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	})
}

var _ Backend = (*HelperClient)(nil)

// call performs one request/response round-trip and decodes the result into out
// (out may be nil for ops with no result body).
func (c *HelperClient) call(ctx context.Context, op Op, payload any, out any) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return fmt.Errorf("linuxpod helper %s: dial: %w", op, err)
	}
	defer conn.Close()

	// Honor the context deadline on the socket I/O so a hung helper cannot block a
	// CRI call indefinitely.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	var raw json.RawMessage
	if payload != nil {
		if raw, err = mustRaw(payload); err != nil {
			return err
		}
	}
	reqLine, err := json.Marshal(wireRequest{Op: op, Payload: raw})
	if err != nil {
		return fmt.Errorf("linuxpod helper %s: encode request: %w", op, err)
	}
	reqLine = append(reqLine, '\n')
	if _, err := conn.Write(reqLine); err != nil {
		return fmt.Errorf("linuxpod helper %s: write: %w", op, err)
	}

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return fmt.Errorf("linuxpod helper %s: read response: %w", op, err)
	}
	var resp wireResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("linuxpod helper %s: decode response: %w", op, err)
	}
	if !resp.OK {
		return errorForCode(resp.Code, resp.Error)
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("linuxpod helper %s: decode result: %w", op, err)
		}
	}
	return nil
}

func (c *HelperClient) Ping(ctx context.Context) (HelperInfo, error) {
	var info HelperInfo
	err := c.call(ctx, opPing, nil, &info)
	return info, err
}

func (c *HelperClient) CreatePod(ctx context.Context, spec PodSpec) (PodStatus, error) {
	var st PodStatus
	err := c.call(ctx, opCreatePod, spec, &st)
	return st, err
}

func (c *HelperClient) PodStatus(ctx context.Context, podID string) (PodStatus, error) {
	var st PodStatus
	err := c.call(ctx, opPodStatus, struct {
		PodID string `json:"podID"`
	}{PodID: podID}, &st)
	return st, err
}

func (c *HelperClient) PrepareContainerRootfs(ctx context.Context, req RootfsRequest) (RootfsHandle, error) {
	var h RootfsHandle
	err := c.call(ctx, opPrepareRootfs, req, &h)
	return h, err
}

func (c *HelperClient) CreateContainer(ctx context.Context, req CreateRequest) (ContainerStatus, error) {
	var st ContainerStatus
	err := c.call(ctx, opCreateContainer, req, &st)
	return st, err
}

func (c *HelperClient) StartContainer(ctx context.Context, ref Ref) (ContainerStatus, error) {
	var st ContainerStatus
	err := c.call(ctx, opStartContainer, ref, &st)
	return st, err
}

func (c *HelperClient) StopContainer(ctx context.Context, req StopRequest) (ContainerStatus, error) {
	var st ContainerStatus
	err := c.call(ctx, opStopContainer, req, &st)
	return st, err
}

func (c *HelperClient) RemoveContainer(ctx context.Context, ref Ref) error {
	return c.call(ctx, opRemoveContainer, ref, nil)
}

func (c *HelperClient) Status(ctx context.Context, ref Ref) (ContainerStatus, error) {
	var st ContainerStatus
	err := c.call(ctx, opStatus, ref, &st)
	return st, err
}

func (c *HelperClient) ContainerLogPath(ctx context.Context, ref Ref) (LogInfo, error) {
	var info LogInfo
	err := c.call(ctx, opContainerLog, ref, &info)
	return info, err
}

func (c *HelperClient) ExecSync(ctx context.Context, req ExecRequest) (ExecResult, error) {
	var res ExecResult
	err := c.call(ctx, opExecSync, req, &res)
	return res, err
}

func (c *HelperClient) ContainerStats(ctx context.Context, ref Ref) (ContainerStats, error) {
	var st ContainerStats
	err := c.call(ctx, opContainerStats, ref, &st)
	return st, err
}

func (c *HelperClient) Cleanup(ctx context.Context, podID string) (CleanupReport, error) {
	var rep CleanupReport
	err := c.call(ctx, opCleanup, struct {
		PodID string `json:"podID"`
	}{PodID: podID}, &rep)
	return rep, err
}
