//go:build darwin

package criserver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"syscall"
)

func setPortForwardDialerInterface(dialer *net.Dialer, iface string) error {
	if iface == "" {
		return nil
	}
	netif, err := net.InterfaceByName(iface)
	if err != nil {
		return err
	}
	dialer.Control = func(network, address string, conn syscall.RawConn) error {
		var controlErr error
		if err := conn.Control(func(fd uintptr) {
			controlErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_BOUND_IF, netif.Index)
		}); err != nil {
			return err
		}
		return controlErr
	}
	return nil
}

func portForwardDialFallback(ctx context.Context, stream io.ReadWriteCloser, host string, port int, cause error) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "/usr/bin/nc", host, strconv.Itoa(port))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("prepare nc stdin after %v: %w", cause, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("prepare nc stdout after %v: %w", cause, err)
	}
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start nc after %v: %w", cause, err)
	}

	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(stdin, stream)
		_ = stdin.Close()
		done <- err
	}()
	go func() {
		_, err := io.Copy(stream, stdout)
		done <- err
	}()

	first := <-done
	_ = stdin.Close()
	_ = stream.Close()
	second := <-done
	waitErr := cmd.Wait()
	if first != nil && first != io.EOF {
		return first
	}
	if second != nil && second != io.EOF {
		return second
	}
	if waitErr != nil {
		return fmt.Errorf("%w: %s", waitErr, stderr.String())
	}
	return nil
}
