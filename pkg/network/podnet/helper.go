package podnet

import (
	"context"

	"github.com/chimerakang/macvz/pkg/network/privhelper"
)

// helperRunner implements runner by delegating to the root privileged helper
// (cmd/macvz-netd) over its unix socket, so the user-run kubelet can program pf
// without itself being root (#38).
type helperRunner struct {
	client *privhelper.Client
}

func (h helperRunner) run(ctx context.Context, c command) (string, error) {
	stdout, stderr, code, err := h.client.Run(ctx, c.Name, c.Args, c.Stdin)
	if err != nil {
		return stdout, &CommandError{Cmd: c.String(), Stderr: stderr, ExitCode: code, Err: err}
	}
	if code != 0 {
		return stdout, &CommandError{Cmd: c.String(), Stderr: stderr, ExitCode: code}
	}
	return stdout, nil
}

// WithHelperSocket routes the Router's privileged commands through the root
// helper daemon at socketPath instead of executing them directly. Use this when
// the kubelet runs as a non-root user (#38).
func WithHelperSocket(socketPath string) Option {
	return func(rt *Router) {
		rt.run = helperRunner{client: privhelper.NewClient(socketPath)}
	}
}
