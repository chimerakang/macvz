package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/chimerakang/macvz/pkg/bootstrap"
	"github.com/chimerakang/macvz/pkg/config"
	"github.com/chimerakang/macvz/pkg/network/wireguard"
)

// dispatchSubcommand runs a bootstrap/doctor subcommand when args[0] names one.
// It returns handled=true (with an exit code) when it consumed the args, or
// handled=false to let the default kubelet path run. Subcommands are recognized
// only as the first argument and only when it is not a flag, so existing
// `macvz-kubelet --config ...` invocations are unaffected.
func dispatchSubcommand(args []string) (handled bool, code int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "doctor":
		return true, runDoctor(args[1:])
	case "bootstrap":
		return true, runBootstrap(args[1:])
	case "bundle":
		return true, runBundle(args[1:])
	case "cleanup":
		return true, runCleanup(args[1:])
	case "remove":
		return true, runRemove(args[1:])
	default:
		return false, 0
	}
}

// runDoctor verifies a node's join prerequisites and prints a per-check report.
// Exit code 0 means clear to join (warnings allowed); 1 means a required
// prerequisite is missing.
func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to the macvz-kubelet config to verify")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doctor: load config: %v\n", err)
		return 1
	}
	internalIP := cfg.Node.InternalIP
	if internalIP == "" {
		internalIP = detectInternalIP()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result := bootstrap.NewDoctor(cfg, internalIP).Run(ctx)
	printDoctorReport(result, cfg.NodeName)
	if result.OK() {
		return 0
	}
	return 1
}

func printDoctorReport(r bootstrap.Result, nodeName string) {
	fmt.Printf("MacVz node preflight — %s\n\n", nodeName)
	var fails, warns int
	for _, c := range r.Checks {
		fmt.Printf("[%-4s] %s\n", c.Status, c.Name)
		if c.Detail != "" {
			fmt.Printf("        %s\n", c.Detail)
		}
		if c.Status != bootstrap.StatusOK && c.Remediation != "" {
			fmt.Printf("        → %s\n", c.Remediation)
		}
		switch c.Status {
		case bootstrap.StatusFail:
			fails++
		case bootstrap.StatusWarn:
			warns++
		}
	}
	fmt.Println()
	if fails > 0 {
		fmt.Printf("NOT READY: %d missing prerequisite(s), %d warning(s). Fix the FAIL items above before starting macvz-kubelet.\n", fails, warns)
		return
	}
	if warns > 0 {
		fmt.Printf("READY with %d warning(s). The node can join; warnings note degraded or runtime-confirmed capabilities.\n", warns)
		return
	}
	fmt.Println("READY: all prerequisites satisfied.")
}

// flagSet reports whether name was explicitly provided on the command line.
func flagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// runBootstrap generates a node config from the minimum join inputs, optionally
// generating a WireGuard keypair and self-signed serving TLS first.
func runBootstrap(args []string) int {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	var (
		nodeName     = fs.String("node-name", "", "Kubernetes node name (required)")
		internalIP   = fs.String("internal-ip", "", "node's reachable IPv4 (required)")
		kubeconfig   = fs.String("kubeconfig", "", "path to the cluster kubeconfig (required)")
		podCIDR      = fs.String("pod-cidr", "", "manual Pod CIDR (only for clusters without node CIDRs)")
		clusterDNS   = fs.String("cluster-dns", "", "CoreDNS/kube-dns ClusterIP for in-VM Service DNS")
		helperSocket = fs.String("helper-socket", "", "macvz-netd socket (required with mesh/podnet)")
		out          = fs.String("out", "", "write config to this file (default: stdout)")

		meshAddr  = fs.String("mesh-address", "", "this node's mesh address in CIDR form, e.g. 10.99.0.1/32 (enables mesh)")
		meshIface = fs.String("mesh-interface", "", "WireGuard interface name (default utun7)")
		genKey    = fs.String("gen-key", "", "generate a WireGuard keypair at this path and print the public key")

		podnetIface = fs.String("podnet-interface", "", "host vmnet bridge, e.g. bridge100 (enables Pod network)")

		genTLS  = fs.Bool("gen-tls", false, "generate a self-signed kubelet serving cert/key")
		tlsCert = fs.String("serving-cert", "/etc/macvz/pki/kubelet.crt", "serving cert path (used with --gen-tls or set directly)")
		tlsKey  = fs.String("serving-key", "/etc/macvz/pki/kubelet.key", "serving key path")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	p := bootstrap.JoinParams{
		NodeName:               *nodeName,
		InternalIP:             *internalIP,
		KubeconfigPath:         *kubeconfig,
		PodCIDR:                *podCIDR,
		PrivilegedHelperSocket: *helperSocket,
	}
	if *clusterDNS != "" {
		p.ClusterDNS = []string{*clusterDNS}
	}

	// WireGuard keypair generation: create the private key on disk and surface
	// the public key so the operator can paste it into peers on the other nodes.
	if *genKey != "" {
		key, err := wireguard.LoadOrCreateKey(*genKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bootstrap: generate WireGuard key: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "WireGuard private key: %s\nWireGuard public key (give to peers): %s\n\n", *genKey, key.PublicKey())
	}

	if *meshAddr != "" {
		p.Mesh = &bootstrap.MeshParams{
			Interface:      *meshIface,
			Address:        *meshAddr,
			PrivateKeyFile: *genKey, // empty falls back to the default in the generator
		}
	}
	if *podnetIface != "" {
		p.PodNetwork = &bootstrap.PodNetworkParams{Interface: *podnetIface}
	}

	if *genTLS {
		if err := bootstrap.GenerateServingTLS(*nodeName, *internalIP, *tlsCert, *tlsKey, 365*24*time.Hour, time.Now()); err != nil {
			fmt.Fprintf(os.Stderr, "bootstrap: generate serving TLS: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "serving TLS written: %s, %s\n\n", *tlsCert, *tlsKey)
	}
	// Reference the serving cert/key in the config only when the operator opted
	// in: either we just generated them, or they explicitly passed one of the
	// serving TLS path flags. The omitted partner falls back to its default path.
	if servingTLSSelected(fs, *genTLS) {
		p.ServingTLSCertFile = *tlsCert
		p.ServingTLSKeyFile = *tlsKey
	}

	yaml, err := bootstrap.GenerateConfig(p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap: %v\n", err)
		return 1
	}

	if *out == "" {
		fmt.Print(yaml)
		return 0
	}
	if err := os.WriteFile(*out, []byte(yaml), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap: write %s: %v\n", *out, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "config written: %s\nnext: macvz-kubelet doctor --config %s\n", *out, *out)
	return 0
}

func servingTLSSelected(fs *flag.FlagSet, genTLS bool) bool {
	return genTLS || flagSet(fs, "serving-cert") || flagSet(fs, "serving-key")
}
