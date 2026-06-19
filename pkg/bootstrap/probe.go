package bootstrap

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"time"
)

// readForwardingSysctl returns the current value of net.inet.ip.forwarding by
// shelling out to sysctl. The Doctor treats a non-1 value as a warning since
// macvz-netd enables forwarding at startup when the Pod network is on.
func readForwardingSysctl() (string, error) {
	out, err := exec.Command("sysctl", "-n", "net.inet.ip.forwarding").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// dialAPIServer confirms the Kubernetes API server host is reachable at the TCP
// layer. It does no TLS handshake or auth — that is the kubelet's job — but a
// successful dial proves the address resolves and the port answers, which is
// the prerequisite most likely to be missing on a fresh node (wrong IP, mesh
// not up, firewall). host is the rest.Config.Host URL (e.g. https://1.2.3.4:6443).
func dialAPIServer(ctx context.Context, host string) error {
	u, err := url.Parse(host)
	if err != nil {
		return fmt.Errorf("parse API server host %q: %w", host, err)
	}
	hostport := u.Host
	if hostport == "" {
		// Host without scheme (rare); use it verbatim.
		hostport = host
	}
	if _, _, err := net.SplitHostPort(hostport); err != nil {
		// Default to the HTTPS API port when none is present.
		hostport = net.JoinHostPort(hostport, "443")
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", hostport)
	if err != nil {
		return err
	}
	return conn.Close()
}
