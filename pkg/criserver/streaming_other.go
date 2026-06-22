//go:build !darwin

package criserver

import (
	"context"
	"io"
	"net"
)

func setPortForwardDialerInterface(_ *net.Dialer, _ string) error {
	return nil
}

func portForwardDialFallback(_ context.Context, _ io.ReadWriteCloser, _ string, _ int, cause error) error {
	return cause
}
