package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"github.com/chimerakang/macvz/pkg/criserver"
	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/runtime/linuxpod"
	"k8s.io/klog/v2"
)

// linuxpod_diagnose.go drives the `macvz-cri --diagnose-linuxpod` operator
// diagnostic (CRI-L6-2, #136). It loads the persisted sandbox/container stores
// read-only and, when a helper socket is configured, probes the live LinuxPod
// helper backend, then prints a machine-readable JSON residual-state report to
// stdout. It never serves CRI and never mutates records, IP reservations, or host
// routes — repair is the live service's job. It is the scriptable counterpart of
// the in-process LinuxPodService.Diagnose used while serving.

// runLinuxPodDiagnose loads the persisted CRI stores under stateDir, optionally
// connects to the LinuxPod helper, and writes a JSON residual-state report to out.
func runLinuxPodDiagnose(ctx context.Context, out io.Writer, stateDir string, lc linuxpodConfig) error {
	report, err := collectLinuxPodResidualReport(ctx, stateDir, lc)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("encode LinuxPod residual report: %w", err)
	}
	return nil
}

// collectLinuxPodResidualReport is the reusable guts of --diagnose-linuxpod: it
// loads the persisted CRI stores under stateDir read-only, optionally probes the
// live LinuxPod helper, and returns the residual-state report. It is shared by
// the standalone diagnostic above and the --support-bundle collector (CRI-L9-3,
// #151) so the two never drift.
func collectLinuxPodResidualReport(ctx context.Context, stateDir string, lc linuxpodConfig) (criserver.LinuxPodResidualReport, error) {
	sandboxes, skipped, err := store.New(stateDir)
	if err != nil {
		return criserver.LinuxPodResidualReport{}, fmt.Errorf("open sandbox store: %w", err)
	}
	if skipped > 0 {
		klog.InfoS("skipped unparseable sandbox records during diagnostic", "count", skipped, "stateDir", stateDir)
	}
	// Container records live in a sibling directory, matching run(); an empty
	// stateDir keeps both in-memory and reports nothing.
	containerDir := stateDir
	if containerDir != "" {
		containerDir = filepath.Join(stateDir, "containers")
	}
	containers, cSkipped, err := store.NewContainerStore(containerDir)
	if err != nil {
		return criserver.LinuxPodResidualReport{}, fmt.Errorf("open container store: %w", err)
	}
	if cSkipped > 0 {
		klog.InfoS("skipped unparseable container records during diagnostic", "count", cSkipped, "stateDir", containerDir)
	}

	// Probe the live helper only when a socket is configured. Without it the report
	// honestly marks every sandbox's backend liveness "unprobed" rather than
	// guessing. A configured helper socket is handshaken first, even if
	// --experimental-linuxpod-backend is not set: diagnose mode is read-only, and the
	// flag help documents --linuxpod-helper-socket as sufficient for a live probe.
	// This also catches stale helper binaries/protocol mismatches before the report
	// turns every sandbox into a vague backend-error bucket.
	var backend linuxpod.Backend
	if lc.helperSocket != "" {
		if _, _, err := (linuxpodConfig{enabled: true, helperSocket: lc.helperSocket}).handshake(ctx); err != nil {
			return criserver.LinuxPodResidualReport{}, fmt.Errorf("probe LinuxPod helper for diagnostic: %w", err)
		}
		backend = linuxpod.NewSocketClient(lc.helperSocket)
	}

	return criserver.DiagnoseLinuxPodStores(ctx, sandboxes, containers, backend), nil
}
