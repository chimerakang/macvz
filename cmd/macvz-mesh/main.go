// Command macvz-mesh is the operator helper for cross-host WireGuard mesh setup
// (issue #55). It automates the parts of mesh configuration that are easy to get
// wrong by hand: generating a stable per-node private key, exporting a node's
// public identity safely, and rendering peer config snippets for other nodes —
// so operators never copy private keys around or hand-edit base64 keys.
//
// Subcommands:
//
//	macvz-mesh keygen --key <file>
//	    Generate (or load) this node's stable private key and print its public
//	    key. The private key file is created with 0600 permissions and never
//	    leaves the node.
//
//	macvz-mesh export --config <file> [--out <file>]
//	    Derive this node's shareable mesh metadata (public key, endpoint, mesh
//	    address, Pod CIDR) from its config and write it as a YAML document. The
//	    document contains no private key and is safe to hand to other nodes.
//
//	macvz-mesh peer [--format macvz|wg] [--keepalive N] <metadata.yaml>...
//	    Turn one or more exported metadata documents into a peer config snippet
//	    to paste into the OTHER nodes' configs.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/chimerakang/macvz/internal/version"
	"github.com/chimerakang/macvz/pkg/config"
	"github.com/chimerakang/macvz/pkg/network/wireguard"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "macvz-mesh:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("a subcommand is required")
	}
	switch args[0] {
	case "keygen":
		return runKeygen(args[1:])
	case "export":
		return runExport(args[1:])
	case "peer":
		return runPeer(args[1:])
	case "version", "--version", "-version":
		fmt.Println(version.String())
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `macvz-mesh — WireGuard mesh key and config helper

Usage:
  macvz-mesh keygen --key <file>
  macvz-mesh export --config <file> [--out <file>]
  macvz-mesh peer [--format macvz|wg] [--keepalive N] <metadata.yaml>...

Run "macvz-mesh <subcommand> -h" for subcommand flags.
`)
}

// runKeygen loads or creates this node's stable private key and prints its
// public key. The private key is persisted with 0600 perms by LoadOrCreateKey,
// so re-running keygen is idempotent and never rotates the key by accident.
func runKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	keyPath := fs.String("key", "", "path to the node's private key file (created if absent)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keyPath == "" {
		return errors.New("keygen: --key is required")
	}

	existed := fileExists(*keyPath)
	key, err := wireguard.LoadOrCreateKey(*keyPath)
	if err != nil {
		return err
	}
	if existed {
		fmt.Fprintf(os.Stderr, "loaded existing private key from %s\n", *keyPath)
	} else {
		fmt.Fprintf(os.Stderr, "generated new private key at %s (mode 0600)\n", *keyPath)
	}
	// The public key goes to stdout alone so it is easy to capture in scripts.
	fmt.Println(key.PublicKey().String())
	return nil
}

// runExport derives and writes this node's shareable mesh metadata.
func runExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to the node's macvz-kubelet config")
	outPath := fs.String("out", "", "write metadata here (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return errors.New("export: --config is required")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	md, err := cfg.ExportMeshMetadata()
	if err != nil {
		return err
	}
	data, err := md.Marshal()
	if err != nil {
		return err
	}

	if *outPath == "" {
		_, err = os.Stdout.Write(data)
		return err
	}
	// Metadata is public, but 0644 keeps it tidy and predictable.
	if err := os.WriteFile(*outPath, data, 0o644); err != nil {
		return fmt.Errorf("write metadata to %q: %w", *outPath, err)
	}
	fmt.Fprintf(os.Stderr, "wrote mesh metadata for %q to %s\n", md.Name, *outPath)
	return nil
}

// runPeer renders peer snippets from one or more exported metadata documents.
func runPeer(args []string) error {
	fs := flag.NewFlagSet("peer", flag.ContinueOnError)
	format := fs.String("format", "macvz", "output format: macvz (config peers:) or wg ([Peer] blocks)")
	keepalive := fs.Int("keepalive", config.DefaultPeerKeepalive, "persistentKeepalive seconds for each peer (0 disables)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	files := fs.Args()
	if len(files) == 0 {
		return errors.New("peer: at least one metadata file is required")
	}
	if *keepalive < 0 {
		return errors.New("peer: --keepalive must not be negative")
	}

	nodes := make([]config.MeshNodeMetadata, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read metadata %q: %w", f, err)
		}
		md, err := config.ParseMeshNodeMetadata(data)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		nodes = append(nodes, md)
	}

	var (
		out string
		err error
	)
	switch *format {
	case "macvz":
		out, err = config.RenderMeshPeers(nodes, *keepalive)
	case "wg":
		out, err = config.RenderWireGuardPeers(nodes, *keepalive)
	default:
		return fmt.Errorf("peer: unknown --format %q (want macvz or wg)", *format)
	}
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(filepath.Clean(path))
	return err == nil
}
