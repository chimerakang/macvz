package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/chimerakang/macvz/internal/version"
	"github.com/chimerakang/macvz/pkg/criserver/store"
	"github.com/chimerakang/macvz/pkg/diagbundle"
	"github.com/chimerakang/macvz/pkg/network/privhelper"
	"k8s.io/klog/v2"
)

// bundle.go implements `macvz-cri --support-bundle` (CRI-L9-3, #151): a redacted
// diagnostic bundle for the CRI/LinuxPod node path, the counterpart of
// `macvz-kubelet bundle` for the Virtual Kubelet path. It collects adapter
// metadata, LinuxPod helper handshake/journals, persisted sandbox/container
// store summaries, macvz-netd status, socket health, and operator-named logs
// into a timestamped directory (and tar.gz unless --no-archive).
//
// Everything flows through pkg/diagbundle's Builder, so every byte is redacted
// before it touches disk, and every source is fail-soft: a broken helper or a
// missing journal records a ".error" sidecar instead of aborting the bundle —
// the broken subsystem is usually the thing being debugged.

// supportBundleTimeout bounds the whole collection so an unresponsive helper
// socket cannot stall the bundle.
const supportBundleTimeout = 60 * time.Second

// bundleLogTailBytes is how much of each --bundle-log-file is kept (the tail),
// so a multi-gigabyte adapter log does not balloon the bundle.
const bundleLogTailBytes = 500 * 1024

// supportBundleConfig collects everything --support-bundle needs: its own
// flags (out dir, extra logs, archive toggle, helper work dir) plus the serving
// flags it reuses to find state and sockets.
type supportBundleConfig struct {
	outDir        string
	logFiles      []string
	noArchive     bool
	helperWorkDir string

	listen        string
	stateDir      string
	streamingAddr string
	lc            linuxpodConfig
	pn            podNetConfig
}

// defaultBundleOut is the --bundle-out default: a timestamped directory under
// the current working directory.
func defaultBundleOut(now time.Time) string {
	return "./macvz-cri-bundle-" + now.UTC().Format("20060102T150405Z")
}

// runSupportBundle collects the bundle and prints the final output path. It
// returns an error only when the bundle itself could not be written; individual
// source failures are recorded inside the bundle (fail-soft) and still exit 0.
func runSupportBundle(ctx context.Context, cfg supportBundleConfig) error {
	outDir := cfg.outDir
	if outDir == "" {
		outDir = defaultBundleOut(time.Now())
	}

	ctx, cancel := context.WithTimeout(ctx, supportBundleTimeout)
	defer cancel()

	b := diagbundle.NewBuilder()
	addSupportBundleSources(b, cfg)

	res, err := b.BuildInto(ctx, outDir, "")
	if err != nil {
		return fmt.Errorf("write support bundle: %w", err)
	}

	final := res.Dir
	if !cfg.noArchive {
		archive := strings.TrimSuffix(res.Dir, "/") + ".tar.gz"
		if err := diagbundle.Archive(res.Dir, archive); err != nil {
			klog.ErrorS(err, "archive support bundle; the directory is still usable", "dir", res.Dir)
		} else {
			_ = os.RemoveAll(res.Dir)
			final = archive
		}
	}

	var failed int
	for _, f := range res.Files {
		if f.Err != "" {
			failed++
		}
	}
	fmt.Println(final)
	klog.InfoS("support bundle written", "path", final,
		"sources", len(res.Files), "sourceErrors", failed,
		"note", "secrets were redacted, but review the contents before sharing")
	return nil
}

// addSupportBundleSources registers every collector. Helper-scoped sources are
// added only when their socket/dir flag is set, so a plain apple/container node's
// bundle is not cluttered with unconfigured-LinuxPod errors.
func addSupportBundleSources(b *diagbundle.Builder, cfg supportBundleConfig) {
	// --- meta ------------------------------------------------------------------
	b.Add("meta/version.txt", func(context.Context) ([]byte, error) {
		host, _ := os.Hostname()
		return []byte(fmt.Sprintf("macvz-cri %s\nos/arch: %s/%s\nhost: %s\ngenerated: %s\n",
			version.String(), runtime.GOOS, runtime.GOARCH, host, time.Now().UTC().Format(time.RFC3339))), nil
	})
	b.Add("meta/args.txt", func(context.Context) ([]byte, error) {
		// One argument per line; the Builder's redactor sanitizes any inline
		// secret-bearing flag value (token=..., etc.) before this hits disk.
		return []byte(strings.Join(os.Args, "\n") + "\n"), nil
	})

	// --- LinuxPod helper ---------------------------------------------------------
	if cfg.lc.helperSocket != "" {
		b.Add("linuxpod/helper-info.json", func(ctx context.Context) ([]byte, error) {
			info, _, err := (linuxpodConfig{enabled: true, helperSocket: cfg.lc.helperSocket}).handshake(ctx)
			if err != nil {
				return nil, err
			}
			return json.MarshalIndent(info, "", "  ")
		})
	}
	b.Add("linuxpod/residual-state.json", func(ctx context.Context) ([]byte, error) {
		report, err := collectLinuxPodResidualReport(ctx, cfg.stateDir, cfg.lc)
		if err != nil {
			return nil, err
		}
		return json.MarshalIndent(report, "", "  ")
	})
	if cfg.helperWorkDir != "" {
		b.Add("linuxpod/supervisor-journal.json", fileBundleSource(filepath.Join(cfg.helperWorkDir, "supervisor-journal.json")))
		b.Add("linuxpod/adoption-journal.json", fileBundleSource(filepath.Join(cfg.helperWorkDir, "adoption-journal.json")))
		b.Add("linuxpod/helper-workdir.txt", func(context.Context) ([]byte, error) {
			return listDirTree(cfg.helperWorkDir)
		})
	}

	// --- persisted CRI state -----------------------------------------------------
	b.Add("state/sandboxes.txt", func(context.Context) ([]byte, error) {
		return summarizeSandboxStore(cfg.stateDir)
	})
	b.Add("state/containers.txt", func(context.Context) ([]byte, error) {
		return summarizeContainerStore(cfg.stateDir)
	})

	// --- network helper ----------------------------------------------------------
	if cfg.pn.helperSocket != "" {
		b.Add("network/netd-status.json", func(ctx context.Context) ([]byte, error) {
			st, err := privhelper.NewClient(cfg.pn.helperSocket).Status(ctx)
			if err != nil {
				return nil, err
			}
			return json.MarshalIndent(st, "", "  ")
		})
	}

	// --- sockets -----------------------------------------------------------------
	b.Add("net/sockets.txt", func(context.Context) ([]byte, error) {
		return describeSockets(cfg), nil
	})

	// --- logs --------------------------------------------------------------------
	for _, lf := range cfg.logFiles {
		b.Add("logs/"+filepath.Base(lf), tailFileSource(lf, bundleLogTailBytes))
	}
}

// summarizeSandboxStore loads the persisted sandbox store read-only and renders
// a per-record summary line (IDs, identity, state, timestamps, Pod IP) — not the
// full spec dump, which can carry more than an issue needs.
func summarizeSandboxStore(stateDir string) ([]byte, error) {
	sandboxes, skipped, err := store.New(stateDir)
	if err != nil {
		return nil, fmt.Errorf("open sandbox store: %w", err)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "state dir: %s\nunparseable records skipped: %d\n\n", emptyDashCRI(stateDir), skipped)
	list := sandboxes.List()
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
	for i := range list {
		s := &list[i]
		fmt.Fprintf(&sb, "%s  %s/%s  state=%s  created=%s  notReady=%s  podIP=%s  vmIP=%s  attached=%t  linuxpodNS=%s\n",
			s.ID, s.Metadata.Namespace, s.Metadata.Name, s.State,
			nanoTime(s.CreatedAt), nanoTime(s.NotReadyAt),
			emptyDashCRI(s.Network.PodIP), emptyDashCRI(s.Network.VMIP), s.Network.Attached,
			emptyDashCRI(s.LinuxPodNamespace))
	}
	if len(list) == 0 {
		sb.WriteString("(no sandbox records)\n")
	}
	return []byte(sb.String()), nil
}

// summarizeContainerStore is summarizeSandboxStore's container counterpart.
func summarizeContainerStore(stateDir string) ([]byte, error) {
	containerDir := stateDir
	if containerDir != "" {
		containerDir = filepath.Join(stateDir, "containers")
	}
	containers, skipped, err := store.NewContainerStore(containerDir)
	if err != nil {
		return nil, fmt.Errorf("open container store: %w", err)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "container dir: %s\nunparseable records skipped: %d\n\n", emptyDashCRI(containerDir), skipped)
	list := containers.List()
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
	for i := range list {
		c := &list[i]
		backend := "-"
		if c.LinuxPod != nil {
			backend = c.LinuxPod.BackendContainerID
		}
		fmt.Fprintf(&sb, "%s  sandbox=%s  name=%s  image=%s  state=%s  created=%s  started=%s  finished=%s  exit=%d  backendContainer=%s\n",
			c.ID, c.SandboxID, c.Metadata.Name, c.Image, c.State,
			nanoTime(c.CreatedAt), nanoTime(c.StartedAt), nanoTime(c.FinishedAt), c.ExitCode, backend)
	}
	if len(list) == 0 {
		sb.WriteString("(no container records)\n")
	}
	return []byte(sb.String()), nil
}

// describeSockets reports existence/mode/age of the CRI socket, the LinuxPod
// helper socket, and the macvz-netd socket, plus the streaming addr config —
// enough to tell "adapter never started" from "helper gone" at a glance.
func describeSockets(cfg supportBundleConfig) []byte {
	var sb strings.Builder
	criPath, err := socketPath(cfg.listen)
	if err != nil {
		fmt.Fprintf(&sb, "cri socket (--listen=%s): invalid endpoint: %v\n", cfg.listen, err)
	} else {
		fmt.Fprintf(&sb, "cri socket (--listen): %s\n", describePath(criPath))
	}
	if cfg.lc.helperSocket != "" {
		fmt.Fprintf(&sb, "linuxpod helper socket: %s\n", describePath(cfg.lc.helperSocket))
	} else {
		sb.WriteString("linuxpod helper socket: (not configured)\n")
	}
	if cfg.pn.helperSocket != "" {
		fmt.Fprintf(&sb, "macvz-netd socket: %s\n", describePath(cfg.pn.helperSocket))
	} else {
		sb.WriteString("macvz-netd socket: (not configured)\n")
	}
	fmt.Fprintf(&sb, "streaming addr (--streaming-addr): %s\n", emptyOrCRI(cfg.streamingAddr, "(disabled)"))
	return []byte(sb.String())
}

// describePath renders one path's existence, mode, and age for net/sockets.txt.
func describePath(path string) string {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Sprintf("%s  MISSING (%v)", path, err)
	}
	return fmt.Sprintf("%s  mode=%s  age=%s", path, info.Mode(), time.Since(info.ModTime()).Round(time.Second))
}

// listDirTree renders a recursive names+sizes listing of dir (no contents), so
// residue like leftover sup-* per-pod directories is visible without dumping
// per-pod files into the bundle.
func listDirTree(dir string) ([]byte, error) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "listing of %s (names and sizes only):\n", dir)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			fmt.Fprintf(&sb, "  %s  ERROR: %v\n", path, err)
			return nil // keep walking; a broken entry is itself evidence
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil || rel == "." {
			return nil
		}
		if d.IsDir() {
			fmt.Fprintf(&sb, "  %s/\n", rel)
			return nil
		}
		var size int64 = -1
		if info, ierr := d.Info(); ierr == nil {
			size = info.Size()
		}
		fmt.Fprintf(&sb, "  %s  %d bytes\n", rel, size)
		return nil
	})
	if err != nil {
		return []byte(sb.String()), fmt.Errorf("walk %s: %w", dir, err)
	}
	return []byte(sb.String()), nil
}

// fileBundleSource reads a file verbatim (redaction is applied by the Builder).
// A missing/unreadable file is recorded as a source error.
func fileBundleSource(path string) func(ctx context.Context) ([]byte, error) {
	return func(context.Context) ([]byte, error) {
		return os.ReadFile(path)
	}
}

// tailFileSource keeps only the last maxBytes of a log file, prefixed with a
// truncation note so a reviewer knows the head was dropped.
func tailFileSource(path string, maxBytes int64) func(ctx context.Context) ([]byte, error) {
	return func(context.Context) ([]byte, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		var header string
		if info.Size() > maxBytes {
			if _, err := f.Seek(info.Size()-maxBytes, 0); err != nil {
				return nil, err
			}
			header = fmt.Sprintf("[truncated: last %d of %d bytes of %s]\n", maxBytes, info.Size(), path)
		}
		data := make([]byte, 0, min(info.Size(), maxBytes))
		buf := make([]byte, 64*1024)
		for {
			n, rerr := f.Read(buf)
			data = append(data, buf[:n]...)
			if rerr != nil {
				break
			}
		}
		return append([]byte(header), data...), nil
	}
}

// nanoTime renders a unix-nanosecond timestamp, "-" when unset.
func nanoTime(ns int64) string {
	if ns == 0 {
		return "-"
	}
	return time.Unix(0, ns).UTC().Format(time.RFC3339)
}

func emptyDashCRI(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func emptyOrCRI(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
