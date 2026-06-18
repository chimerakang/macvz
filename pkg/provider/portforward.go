package provider

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	vkapi "github.com/virtual-kubelet/virtual-kubelet/node/api"
	"k8s.io/klog/v2"
)

// Compile-time assertion that PortForward satisfies the Virtual Kubelet handler
// wired into the kubelet API server.
var _ vkapi.PortForwardHandlerFunc = (*Provider)(nil).PortForward

// portForwardDialTimeout bounds the connection attempt to the micro-VM.
const portForwardDialTimeout = 10 * time.Second

// PortForward proxies a single `kubectl port-forward` stream to a port inside a
// MacVz-backed Pod.
//
// The kubelet runs on the same Mac as the Pod's micro-VM, so it dials the VM's
// address directly (the host can always reach the guest's vmnet address, with or
// without the cross-host Pod network path). Bytes are copied bidirectionally
// between the Kubernetes stream and the TCP connection until either side closes
// or the context is cancelled; both copy goroutines and the connection are
// always reaped, so closing the forward leaks nothing.
func (p *Provider) PortForward(ctx context.Context, namespace, name string, port int32, stream io.ReadWriteCloser) error {
	key := podKey(namespace, name)
	if port < 1 || port > 65535 {
		return fmt.Errorf("port-forward to pod %q: port %d is out of range", key, port)
	}

	p.mu.RLock()
	st, ok := p.pods[key]
	p.mu.RUnlock()
	if !ok {
		return errdefs.NotFoundf("pod %q is not known to this node", key)
	}

	host, err := p.portForwardTarget(ctx, st, key)
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(host, strconv.Itoa(int(port)))
	dialer := net.Dialer{Timeout: portForwardDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		// Nothing listening on that port inside the Pod, or the guest is
		// unreachable: surface a clear error to kubectl.
		return fmt.Errorf("port-forward to pod %q: dial %s: %w", key, addr, err)
	}
	klog.InfoS("port-forward established", "pod", key, "port", port, "target", addr)

	err = proxyStream(ctx, stream, conn)
	klog.InfoS("port-forward closed", "pod", key, "port", port)
	return err
}

// portForwardTarget resolves the address to dial for a Pod: the live
// runtime-reported micro-VM address, falling back to the last observed VM IP. It
// returns a clear error when the Pod is not running or has no address yet.
func (p *Provider) portForwardTarget(ctx context.Context, st *podState, key string) (string, error) {
	if len(st.workloads) == 0 {
		return "", fmt.Errorf("port-forward to pod %q: pod has no running container", key)
	}
	id := st.workloads[0].id

	rs, err := p.rt.Status(ctx, id)
	if err != nil {
		// Fall back to the address observed at attach time if the runtime probe
		// fails transiently; otherwise the Pod is effectively unreachable.
		if st.vmIP != "" {
			return st.vmIP, nil
		}
		return "", fmt.Errorf("port-forward to pod %q: %w", key, err)
	}
	if rs.Phase != runtime.PhaseRunning {
		return "", fmt.Errorf("port-forward to pod %q: container is not running (%s)", key, rs.Phase)
	}
	if rs.IP != "" {
		return rs.IP, nil
	}
	if st.vmIP != "" {
		return st.vmIP, nil
	}
	return "", fmt.Errorf("port-forward to pod %q: micro-VM has no address yet", key)
}

// proxyStream copies bytes both ways between the port-forward stream and the
// micro-VM connection. It returns when either direction ends or ctx is
// cancelled, then closes both endpoints and waits for both copies to finish so
// no goroutine or connection is leaked.
func proxyStream(ctx context.Context, stream io.ReadWriteCloser, conn net.Conn) error {
	done := make(chan error, 2)
	cp := func(dst io.Writer, src io.Reader) { _, err := io.Copy(dst, src); done <- err }
	go cp(conn, stream)
	go cp(stream, conn)

	var first error
	select {
	case <-ctx.Done():
		first = ctx.Err()
	case first = <-done:
	}

	// Unblock and reap the other direction.
	_ = conn.Close()
	_ = stream.Close()
	<-done

	if first == io.EOF {
		return nil
	}
	return first
}
