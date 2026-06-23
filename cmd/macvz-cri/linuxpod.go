package main

import (
	"context"
	"fmt"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime/linuxpod"
)

// linuxpod.go wires the experimental LinuxPod backend gate (CRI-R17, #124). The
// LinuxPod late-rootfs backend is reached through a helper that speaks the NDJSON
// contract in pkg/runtime/linuxpod. This adapter does not yet serve the full CRI
// surface onto that backend — that is deliberately out of scope for the R17
// prototype (no k3s in-loop, no production claim). What the gate does today is an
// honest, loud startup handshake: when enabled it connects to the helper socket
// and verifies the contract is answerable, so an operator learns immediately
// whether the LinuxPod helper is reachable rather than discovering it mid-Pod.
// The shipped apple/container CRI serving path is unchanged.

// linuxpodConfig collects the experimental LinuxPod backend flags.
type linuxpodConfig struct {
	enabled      bool
	helperSocket string
}

// linuxpodHandshakeTimeout bounds the startup Ping so an unresponsive helper
// fails the adapter fast instead of hanging before it serves.
const linuxpodHandshakeTimeout = 5 * time.Second

// validate checks the flag combination is usable. Enabling the backend without a
// helper socket is a configuration error, reported honestly rather than silently
// ignored.
func (lc linuxpodConfig) validate() error {
	if !lc.enabled {
		return nil
	}
	if lc.helperSocket == "" {
		return fmt.Errorf("--experimental-linuxpod-backend requires --linuxpod-helper-socket to point at a running LinuxPod helper")
	}
	return nil
}

// handshake validates the config and, when the backend is enabled, connects to
// the helper and performs a Ping so a misconfigured or unreachable helper fails
// loudly at startup. It returns the helper info on success. When the backend is
// disabled it is a no-op returning a zero HelperInfo and ok=false.
func (lc linuxpodConfig) handshake(ctx context.Context) (info linuxpod.HelperInfo, ok bool, err error) {
	if err := lc.validate(); err != nil {
		return linuxpod.HelperInfo{}, false, err
	}
	if !lc.enabled {
		return linuxpod.HelperInfo{}, false, nil
	}
	client := linuxpod.NewSocketClient(lc.helperSocket)
	hctx, cancel := context.WithTimeout(ctx, linuxpodHandshakeTimeout)
	defer cancel()
	info, err = client.Ping(hctx)
	if err != nil {
		return linuxpod.HelperInfo{}, false, fmt.Errorf(
			"LinuxPod helper handshake on %s failed: %w; start the helper or pass the correct --linuxpod-helper-socket",
			lc.helperSocket, err)
	}
	return info, true, nil
}
