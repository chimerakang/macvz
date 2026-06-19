// Package e2e's redaction guard. Unlike the cluster-gated suite in
// e2e_test.go (which is behind the `e2e` build tag and MACVZ_E2E=1), this test
// has no build tag and needs no cluster: it exercises the diagnostics
// collector's `redact` filter directly, so the safety property "bundles never
// leak secrets" (issue #43) is verified on every `go test` run.
package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// collectorPath resolves collect-node-diag.sh next to this test.
func collectorPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test source path")
	}
	return filepath.Join(filepath.Dir(thisFile), "collect-node-diag.sh")
}

// runRedact pipes input through `collect-node-diag.sh redact` and returns the
// filtered output.
func runRedact(t *testing.T, input string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	cmd := exec.Command("bash", collectorPath(t), "redact")
	cmd.Stdin = strings.NewReader(input)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("redact filter failed: %v\n%s", err, out.String())
	}
	return out.String()
}

func TestRedactMasksSecrets(t *testing.T) {
	// Sensitive lines that MUST be masked, each paired with the secret token that
	// must not survive.
	secrets := map[string]string{
		"private key: SECRETprivkey0000000000000000000000000000==":    "SECRETprivkey",
		"preshared key: PSKvalue1111111111111111111111111111111111=":  "PSKvalue",
		"PrivateKey = ZZZprivconf2222222222222222222222222222222222=": "ZZZprivconf",
		"  token: abc.def.ghijklmnop":                                 "abc.def.ghijklmnop",
		"client-key-data: BASE64PRIVATEKEYDATA==":                     "BASE64PRIVATEKEYDATA",
		"password: hunter2":                             "hunter2",
		"    authorization: Bearer eyJsupersecrettoken": "eyJsupersecrettoken",
	}
	var input strings.Builder
	for line := range secrets {
		input.WriteString(line + "\n")
	}
	got := runRedact(t, input.String())

	for line, secret := range secrets {
		if strings.Contains(got, secret) {
			t.Errorf("secret leaked from %q: output still contains %q\n%s", line, secret, got)
		}
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected [REDACTED] markers in output:\n%s", got)
	}
}

func TestRedactKeepsDiagnosticContext(t *testing.T) {
	// Non-secret lines a responder needs to keep MUST survive verbatim — most
	// importantly the WireGuard public key, which looks like a private key but is
	// safe and essential for correlating peers.
	keep := []string{
		"peer: kBLohUgv6VLiJAtZ846NrVKTHDmG/bT3qQ6g3D/jgUU=",
		"  latest handshake: 12 seconds ago",
		"  allowed ips: 10.244.102.0/24",
		"address: 10.99.0.1/32",
		"net.inet.ip.forwarding: 1",
		"binat on bridge100 inet from 10.244.101.5 to any -> 192.168.1.110",
	}
	input := strings.Join(keep, "\n") + "\n"
	got := runRedact(t, input)
	for _, line := range keep {
		if !strings.Contains(got, line) {
			t.Errorf("diagnostic context dropped: %q missing from\n%s", line, got)
		}
	}
}

// --- fault injection ---------------------------------------------------------
//
// Issue #43 acceptance: "Force a known network failure and inspect diagnostic
// bundle" — the bundle must produce enough data to tell mesh, route, pf, and
// forwarding failures apart. We can't stand up two Macs in a unit test, so we
// drive the REAL collector against stubbed node commands (wg/pfctl/netstat/
// sysctl/ifconfig shadowed on PATH), inject one failure at a time, and assert
// the bundle distinguishes that failure from a healthy node — and still redacts
// secrets under every scenario.

// nodeState is one node's simulated network state. Each field is the stdout the
// corresponding stubbed command prints.
type nodeState struct {
	wgShow string // `wg show utun7`
	route  string // `netstat -rn -f inet`
	pfNat  string // `pfctl -a macvz/pods -s nat`
	fwd    string // value of net.inet.ip.forwarding (`sysctl`)
}

func healthyState() nodeState {
	return nodeState{
		wgShow: "interface: utun7\n  public key: kBLohUgv6VLiJAtZ846NrVKTHDmG/bT3qQ6g3D/jgUU=\n" +
			"  private key: (hidden)\npeer: PEERpublic111111111111111111111111111111111=\n" +
			"  endpoint: 192.168.1.122:51820\n  allowed ips: 10.244.102.0/24\n  latest handshake: 9 seconds ago\n" +
			"  transfer: 4.20 KiB received, 5.00 KiB sent",
		route: "Routing tables\n\nInternet:\nDestination        Gateway            Flags   Netif\n" +
			"10.99.0.2/32       utun7              USc     utun7\n10.244.102.0/24    utun7              USc     utun7",
		pfNat: "nat-anchor macvz/pods\nbinat on bridge100 inet from 10.244.101.5 to any -> 192.168.1.110",
		fwd:   "1",
	}
}

// writeFakeBins writes stub executables for the node commands into dir, driven
// by env vars so one set of stubs serves every scenario.
func writeFakeBins(t *testing.T, dir string) {
	t.Helper()
	bins := map[string]string{
		"wg": `#!/usr/bin/env bash
case "$1 $2" in
"show interfaces") echo "utun7" ;;
"show utun7")      printf '%s\n' "$FAKE_WG_SHOW" ;;
"showconf utun7")  printf '[Interface]\nPrivateKey = SECRETprivkeyMUSTNOTLEAK00000000000000000=\n[Peer]\nPublicKey = PEERpublic111111111111111111111111111111111=\n' ;;
*) echo "wg: unexpected $*" >&2; exit 1 ;;
esac`,
		"netstat":  `#!/usr/bin/env bash` + "\n" + `printf '%s\n' "$FAKE_ROUTE"`,
		"sysctl":   `#!/usr/bin/env bash` + "\n" + `printf 'net.inet.ip.forwarding: %s\n' "$FAKE_FWD"`,
		"ifconfig": `#!/usr/bin/env bash` + "\n" + `echo "$1: flags=8051<UP,RUNNING> mtu 1420"`,
		"pfctl": `#!/usr/bin/env bash
case "$*" in
*"-s info"*) echo "Status: Enabled" ;;
*"-s nat"*)  printf '%s\n' "$FAKE_PF_NAT" ;;
*"-s rules"*) echo "anchor \"macvz/pods\" all" ;;
*) echo "pfctl: unexpected $*" >&2 ;;
esac`,
	}
	for name, body := range bins {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body+"\n"), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
}

// runCollector runs the real collector with the fake bins shadowing PATH and the
// given node state, returning the (redacted) bundle.
func runCollector(t *testing.T, st nodeState) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	fakeDir := t.TempDir()
	writeFakeBins(t, fakeDir)

	cmd := exec.Command("bash", collectorPath(t))
	cmd.Env = append(os.Environ(),
		"PATH="+fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"MACVZ_DIAG_MESH_IFACE=utun7",
		"FAKE_WG_SHOW="+st.wgShow,
		"FAKE_ROUTE="+st.route,
		"FAKE_PF_NAT="+st.pfNat,
		"FAKE_FWD="+st.fwd,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("collector run failed: %v\n%s", err, out.String())
	}
	return out.String()
}

// sectionOf returns the body of the "### <title>" block in a bundle, up to the
// next "### " header. Scoping assertions to a section matters because the same
// token can legitimately appear in two places — e.g. the remote Pod CIDR shows
// up as a WireGuard allowed-ip AND as a route, and telling "no route" apart from
// "no allowed-ip" is exactly the diagnostic the bundle must support.
func sectionOf(bundle, title string) string {
	lines := strings.Split(bundle, "\n")
	var b strings.Builder
	in := false
	for _, l := range lines {
		if strings.HasPrefix(l, "### ") {
			in = strings.Contains(l, title)
			continue
		}
		if in {
			b.WriteString(l)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func TestDiagnosticBundleDistinguishesFailures(t *testing.T) {
	// Sanity: a healthy node shows every positive signal.
	healthy := runCollector(t, healthyState())
	for _, want := range []string{
		"latest handshake",   // mesh up
		"10.244.102.0/24",    // remote Pod CIDR route present
		"binat on bridge100", // pf binat rule present
		"net.inet.ip.forwarding: 1",
	} {
		if !strings.Contains(healthy, want) {
			t.Fatalf("healthy bundle missing %q:\n%s", want, healthy)
		}
	}

	cases := []struct {
		name   string
		mutate func(*nodeState)
		// gone: a signal that disappears under this failure (so it is detectable).
		gone string
		// goneScope, when set, scopes the gone check to that bundle section so a
		// token that legitimately appears elsewhere doesn't mask the failure.
		goneScope string
		// other signals that MUST remain, proving the failure is localized and
		// not confused with a different failure class.
		stillPresent []string
	}{
		{
			name:         "mesh down (no WireGuard handshake)",
			mutate:       func(s *nodeState) { s.wgShow = strings.Replace(s.wgShow, "  latest handshake: 9 seconds ago\n", "", 1) },
			gone:         "latest handshake",
			stillPresent: []string{"10.244.102.0/24", "binat on bridge100", "net.inet.ip.forwarding: 1"},
		},
		{
			name: "route missing (no remote Pod CIDR via utun7)",
			mutate: func(s *nodeState) {
				s.route = "Routing tables\n\nInternet:\nDestination Gateway Flags Netif\ndefault 192.168.1.1 UGScg en0"
			},
			gone:      "10.244.102.0/24",
			goneScope: "IPv4 routing table",
			// The CIDR is gone from the route table but STILL advertised as a
			// WireGuard allowed-ip — that contrast is what says "route, not mesh".
			stillPresent: []string{"allowed ips: 10.244.102.0/24", "latest handshake", "binat on bridge100", "net.inet.ip.forwarding: 1"},
		},
		{
			name:         "pf anchor empty (no binat — Pod not attached)",
			mutate:       func(s *nodeState) { s.pfNat = "nat-anchor macvz/pods" },
			gone:         "binat on bridge100",
			stillPresent: []string{"latest handshake", "10.244.102.0/24", "net.inet.ip.forwarding: 1"},
		},
		{
			name:         "forwarding disabled",
			mutate:       func(s *nodeState) { s.fwd = "0" },
			gone:         "net.inet.ip.forwarding: 1",
			stillPresent: []string{"latest handshake", "10.244.102.0/24", "binat on bridge100"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := healthyState()
			c.mutate(&st)
			got := runCollector(t, st)

			haystack := got
			if c.goneScope != "" {
				haystack = sectionOf(got, c.goneScope)
			}
			if strings.Contains(haystack, c.gone) {
				t.Errorf("failure %q should remove signal %q from the bundle:\n%s", c.name, c.gone, got)
			}
			for _, sig := range c.stillPresent {
				if !strings.Contains(got, sig) {
					t.Errorf("failure %q wrongly removed unrelated signal %q (can't localize):\n%s", c.name, sig, got)
				}
			}
			// Safety invariant holds under every failure: the private key from
			// `wg showconf` is never leaked.
			if strings.Contains(got, "SECRETprivkeyMUSTNOTLEAK") {
				t.Errorf("private key leaked in %q bundle:\n%s", c.name, got)
			}
			if !strings.Contains(got, "PEERpublic1111") {
				t.Errorf("public key wrongly redacted in %q bundle:\n%s", c.name, got)
			}
		})
	}
}
