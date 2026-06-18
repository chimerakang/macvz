package provider

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/chimerakang/macvz/pkg/runtime"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	vkapi "github.com/virtual-kubelet/virtual-kubelet/node/api"
	"k8s.io/klog/v2"
	utilexec "k8s.io/utils/exec"
)

// Compile-time assertions that the provider methods satisfy the Virtual Kubelet
// log/exec handler function types wired into its kubelet API server.
var (
	_ vkapi.ContainerLogsHandlerFunc = (*Provider)(nil).GetContainerLogs
	_ vkapi.ContainerExecHandlerFunc = (*Provider)(nil).RunInContainer
)

// GetContainerLogs streams a container's output for `kubectl logs`, mapping the
// Virtual Kubelet log options onto runtime.LogOptions. The caller closes the
// returned reader.
func (p *Provider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts vkapi.ContainerLogOpts) (io.ReadCloser, error) {
	id, err := p.workloadID(namespace, podName, containerName)
	if err != nil {
		return nil, err
	}
	rc, err := p.rt.Logs(ctx, id, runtime.LogOptions{
		Follow: opts.Follow,
		Tail:   opts.Tail,
	})
	if err != nil {
		return nil, mapExecLogError(namespace, podName, containerName, err)
	}
	klog.InfoS("streaming container logs", "pod", podKey(namespace, podName), "container", containerName, "follow", opts.Follow, "tail", opts.Tail)
	return rc, nil
}

// RunInContainer executes a command inside a running workload for
// `kubectl exec`, wiring stdin/stdout/stderr and TTY. A non-zero command exit
// is returned as a utilexec.ExitError so the API surfaces the exit status.
func (p *Provider) RunInContainer(ctx context.Context, namespace, podName, containerName string, cmd []string, attach vkapi.AttachIO) error {
	id, err := p.workloadID(namespace, podName, containerName)
	if err != nil {
		return err
	}

	// The runtime does not support terminal resize; drain the resize channel so
	// the API server's producer never blocks.
	if attach.TTY() {
		go drainResize(ctx, attach.Resize())
	}

	sio := runtime.ExecIO{
		Stdin:  attach.Stdin(),
		Stdout: attach.Stdout(),
		Stderr: attach.Stderr(),
		TTY:    attach.TTY(),
	}
	klog.InfoS("exec in container", "pod", podKey(namespace, podName), "container", containerName, "tty", attach.TTY())

	err = p.rt.Exec(ctx, id, cmd, sio)
	if err == nil {
		return nil
	}
	// Convert a clean non-zero exit into an ExitError carrying the code, so
	// kubectl reports "command terminated with exit code N" rather than a
	// generic failure.
	var exit *runtime.ExitError
	if errors.As(err, &exit) {
		return execCodeError{code: exit.Code}
	}
	return mapExecLogError(namespace, podName, containerName, err)
}

// workloadID resolves the runtime workload ID backing a Pod's container,
// returning an errdefs.NotFound error when the Pod, the container, or its
// backing workload is unknown (e.g. the Pod never ran).
func (p *Provider) workloadID(namespace, podName, containerName string) (string, error) {
	key := podKey(namespace, podName)
	p.mu.RLock()
	st, ok := p.pods[key]
	p.mu.RUnlock()
	if !ok {
		return "", errdefs.NotFoundf("pod %q is not known to this node", key)
	}
	for _, w := range st.workloads {
		if w.container == containerName {
			return w.id, nil
		}
	}
	return "", errdefs.NotFoundf("container %q has no running workload in pod %q", containerName, key)
}

// mapExecLogError turns runtime sentinels into Kubernetes-facing errors.
func mapExecLogError(namespace, podName, containerName string, err error) error {
	key := podKey(namespace, podName)
	switch {
	case errors.Is(err, runtime.ErrNotFound):
		return errdefs.NotFoundf("container %q workload not found for pod %q", containerName, key)
	case errors.Is(err, runtime.ErrNotRunning):
		return fmt.Errorf("container %q in pod %q is not running", containerName, key)
	default:
		return err
	}
}

// drainResize discards terminal resize events until the channel closes or the
// context is done.
func drainResize(ctx context.Context, resize <-chan vkapi.TermSize) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-resize:
			if !ok {
				return
			}
		}
	}
}

// execCodeError adapts a command exit code to utilexec.ExitError, the interface
// the Virtual Kubelet exec handler inspects to report the exit status.
type execCodeError struct{ code int }

var _ utilexec.ExitError = execCodeError{}

func (e execCodeError) Error() string {
	return fmt.Sprintf("command terminated with exit code %d", e.code)
}
func (e execCodeError) String() string  { return e.Error() }
func (e execCodeError) Exited() bool    { return true }
func (e execCodeError) ExitStatus() int { return e.code }
