package privhelper

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// samplePolicy mirrors a node configured with one mesh interface, one vmnet
// interface, the managed anchor, and a single peer.
func samplePolicy() Policy {
	return Policy{
		MeshInterface:  "utun7",
		VMNetInterface: "bridge100",
		Anchor:         "macvz/pods",
		MeshAddressIP:  "10.99.0.1",
		MTU:            1380,
		RouteCIDRs:     NormalizeCIDRSet([]string{"10.244.1.0/24", "10.99.0.2/32"}),
		PodCIDRs:       NormalizeCIDRSet([]string{"10.244.1.0/24"}),
		VMNetCIDRs:     NormalizeCIDRSet([]string{"192.168.64.0/24"}),
		PeerPublicKeys: map[string]bool{"PEERKEY00000000000000000000000000000000000=": true},
	}
}

// validAnchorRuleset is what podnet.renderAnchor/renderServiceRules emit.
const validAnchorRuleset = `# Managed by macvz (issue #22). Do not edit by hand.
# default/web
binat on bridge100 from 192.168.64.3 to any -> 10.244.1.5
# default/svc 10.96.0.1:80 -> :8080
rdr on bridge100 inet proto tcp from any to 10.96.0.1 port 80 -> 10.244.1.5 port 8080
`

// validWGConfig is what wireguard.InterfaceConfig.SyncConfig emits for the peer.
const validWGConfig = `[Interface]
PrivateKey = THISNODEPRIVKEY0000000000000000000000000000=
ListenPort = 51820

[Peer]
# node-b
PublicKey = PEERKEY00000000000000000000000000000000000=
Endpoint = 198.51.100.7:51820
AllowedIPs = 10.244.1.0/24, 10.99.0.2/32
`

func TestPolicyAllowsConfiguredCommands(t *testing.T) {
	p := samplePolicy()
	allowed := []struct {
		name string
		desc string
		req  Request
	}{
		{"sysctl", "ping probe", Request{Name: "sysctl", Args: []string{"-n", "kern.ostype"}}},
		{"sysctl", "enable forwarding", Request{Name: "sysctl", Args: []string{"-w", "net.inet.ip.forwarding=1"}}},
		{"pfctl", "enable pf", Request{Name: "pfctl", Args: []string{"-e"}}},
		{"pfctl", "flush managed anchor", Request{Name: "pfctl", Args: []string{"-a", "macvz/pods", "-F", "all"}}},
		{"pfctl", "load managed anchor", Request{Name: "pfctl", Args: []string{"-a", "macvz/pods", "-f", "-"}, Stdin: validAnchorRuleset}},
		{"route", "add configured pod CIDR", Request{Name: "route", Args: []string{"-q", "-n", "add", "-inet", "10.244.1.0/24", "-interface", "utun7"}}},
		{"route", "delete configured mesh CIDR", Request{Name: "route", Args: []string{"-q", "-n", "delete", "-inet", "10.99.0.2/32", "-interface", "utun7"}}},
		{"route", "delete vmnet default route", Request{Name: "route", Args: []string{"-q", "-n", "delete", "-inet", "default", "-interface", "bridge100"}}},
		{"ifconfig", "assign mesh address", Request{Name: "ifconfig", Args: []string{"utun7", "inet", "10.99.0.1", "10.99.0.1", "alias"}}},
		{"ifconfig", "set configured mtu", Request{Name: "ifconfig", Args: []string{"utun7", "mtu", "1380"}}},
		{"ifconfig", "bring up", Request{Name: "ifconfig", Args: []string{"utun7", "up"}}},
		{"ifconfig", "destroy", Request{Name: "ifconfig", Args: []string{"utun7", "destroy"}}},
		{"wg", "setconf managed iface", Request{Name: "wg", Args: []string{"setconf", "utun7", "/dev/stdin"}, Stdin: validWGConfig}},
		{"wg", "syncconf managed iface", Request{Name: "wg", Args: []string{"syncconf", "utun7", "/dev/stdin"}, Stdin: validWGConfig}},
		{"wireguard-go", "create managed iface", Request{Name: "wireguard-go", Args: []string{"utun7"}}},
		{"pkill", "stop managed wireguard-go", Request{Name: "pkill", Args: []string{"-f", "wireguard-go utun7"}}},
	}
	for _, tc := range allowed {
		if err := p.Validate(tc.req); err != nil {
			t.Errorf("%s (%s): want allowed, got refused: %v", tc.name, tc.desc, err)
		}
	}
}

func TestPolicyRefusesOutOfScopeCommands(t *testing.T) {
	p := samplePolicy()
	refused := []struct {
		desc string
		req  Request
	}{
		// sysctl: only the ostype probe and the forwarding toggle.
		{"arbitrary sysctl write", Request{Name: "sysctl", Args: []string{"-w", "kern.maxfiles=99999"}}},
		{"forwarding non-boolean", Request{Name: "sysctl", Args: []string{"-w", "net.inet.ip.forwarding=2"}}},
		{"sysctl read of secret key", Request{Name: "sysctl", Args: []string{"-n", "kern.hostname"}}},
		{"sysctl with stdin", Request{Name: "sysctl", Args: []string{"-n", "kern.ostype"}, Stdin: "x"}},

		// pfctl: only the managed anchor; never the main ruleset.
		{"foreign anchor flush", Request{Name: "pfctl", Args: []string{"-a", "evil/anchor", "-F", "all"}}},
		{"load main ruleset (no anchor)", Request{Name: "pfctl", Args: []string{"-f", "-"}, Stdin: "pass in all\n"}},
		{"flush everything", Request{Name: "pfctl", Args: []string{"-F", "all"}}},
		{"non-binat/rdr rule in anchor", Request{Name: "pfctl", Args: []string{"-a", "macvz/pods", "-f", "-"}, Stdin: "pass in all\n"}},
		{"rule on foreign interface", Request{Name: "pfctl", Args: []string{"-a", "macvz/pods", "-f", "-"}, Stdin: "binat on en0 from 1.2.3.4 to any -> 5.6.7.8\n"}},
		{"binat from foreign vmnet ip", Request{Name: "pfctl", Args: []string{"-a", "macvz/pods", "-f", "-"}, Stdin: "binat on bridge100 from 172.16.0.2 to any -> 10.244.1.5\n"}},
		{"binat to foreign pod ip", Request{Name: "pfctl", Args: []string{"-a", "macvz/pods", "-f", "-"}, Stdin: "binat on bridge100 from 192.168.64.3 to any -> 10.250.1.5\n"}},
		{"rdr to foreign ip", Request{Name: "pfctl", Args: []string{"-a", "macvz/pods", "-f", "-"}, Stdin: "rdr on bridge100 inet proto tcp from any to 10.96.0.1 port 80 -> 1.2.3.4 port 8080\n"}},

		// route: only configured CIDRs through the managed interface.
		{"default route hijack", Request{Name: "route", Args: []string{"-q", "-n", "add", "-inet", "0.0.0.0/0", "-interface", "utun7"}}},
		{"default route add on vmnet", Request{Name: "route", Args: []string{"-q", "-n", "add", "-inet", "default", "-interface", "bridge100"}}},
		{"default route delete on foreign interface", Request{Name: "route", Args: []string{"-q", "-n", "delete", "-inet", "default", "-interface", "en0"}}},
		{"unconfigured CIDR", Request{Name: "route", Args: []string{"-q", "-n", "add", "-inet", "10.250.0.0/24", "-interface", "utun7"}}},
		{"route via foreign interface", Request{Name: "route", Args: []string{"-q", "-n", "add", "-inet", "10.244.1.0/24", "-interface", "en0"}}},
		{"route flush", Request{Name: "route", Args: []string{"-n", "flush"}}},

		// ifconfig: only the managed interface, configured address/mtu.
		{"foreign interface down", Request{Name: "ifconfig", Args: []string{"en0", "down"}}},
		{"wrong mesh address", Request{Name: "ifconfig", Args: []string{"utun7", "inet", "1.2.3.4", "1.2.3.4", "alias"}}},
		{"wrong mtu", Request{Name: "ifconfig", Args: []string{"utun7", "mtu", "9000"}}},
		{"arbitrary ifconfig verb", Request{Name: "ifconfig", Args: []string{"utun7", "delete", "10.99.0.1"}}},

		// wg: only the managed interface, only configured peers and CIDRs.
		{"wg on foreign interface", Request{Name: "wg", Args: []string{"setconf", "en0", "/dev/stdin"}, Stdin: validWGConfig}},
		{"wg unknown peer key", Request{Name: "wg", Args: []string{"setconf", "utun7", "/dev/stdin"}, Stdin: "[Peer]\nPublicKey = ATTACKERKEY000000000000000000000000000000=\nAllowedIPs = 10.244.1.0/24\n"}},
		{"wg AllowedIPs hijack", Request{Name: "wg", Args: []string{"setconf", "utun7", "/dev/stdin"}, Stdin: "[Peer]\nPublicKey = PEERKEY00000000000000000000000000000000000=\nAllowedIPs = 0.0.0.0/0\n"}},

		// wireguard-go: only the managed interface.
		{"wireguard-go foreign iface", Request{Name: "wireguard-go", Args: []string{"en0"}}},

		// pkill: only the managed wireguard-go process pattern.
		{"pkill foreign wireguard-go iface", Request{Name: "pkill", Args: []string{"-f", "wireguard-go en0"}}},
		{"pkill arbitrary process", Request{Name: "pkill", Args: []string{"-f", "ssh"}}},
		{"pkill without -f", Request{Name: "pkill", Args: []string{"wireguard-go utun7"}}},

		// arity / shell-injection-shaped extras.
		{"pfctl extra arg", Request{Name: "pfctl", Args: []string{"-e", "-x"}}},
		{"route trailing arg", Request{Name: "route", Args: []string{"-q", "-n", "add", "-inet", "10.244.1.0/24", "-interface", "utun7", ";reboot"}}},
	}
	for _, tc := range refused {
		if err := p.Validate(tc.req); err == nil {
			t.Errorf("%s: want refused, got allowed", tc.desc)
		}
	}
}

func TestZeroPolicyFailsClosed(t *testing.T) {
	var p Policy // no interfaces, anchors, CIDRs, or peers configured
	// The harmless probe still works so health checks function.
	if err := p.Validate(Request{Name: "sysctl", Args: []string{"-n", "kern.ostype"}}); err != nil {
		t.Errorf("zero policy should allow the ping probe: %v", err)
	}
	// Every mutating command is refused.
	mutations := []Request{
		{Name: "pfctl", Args: []string{"-a", "macvz/pods", "-f", "-"}, Stdin: validAnchorRuleset},
		{Name: "route", Args: []string{"-q", "-n", "add", "-inet", "10.244.1.0/24", "-interface", "utun7"}},
		{Name: "ifconfig", Args: []string{"utun7", "up"}},
		{Name: "wg", Args: []string{"setconf", "utun7", "/dev/stdin"}, Stdin: validWGConfig},
		{Name: "wireguard-go", Args: []string{"utun7"}},
		{Name: "sysctl", Args: []string{"-w", "net.inet.ip.forwarding=1"}},
	}
	for _, req := range mutations {
		if err := p.Validate(req); err == nil {
			t.Errorf("zero policy should refuse %s %v", req.Name, req.Args)
		}
	}
}

func TestPolicyNormalisesCIDRWithHostBits(t *testing.T) {
	p := samplePolicy()
	// 10.244.1.5/24 normalises to the configured 10.244.1.0/24 network.
	req := Request{Name: "route", Args: []string{"-q", "-n", "add", "-inet", "10.244.1.5/24", "-interface", "utun7"}}
	if err := p.Validate(req); err != nil {
		t.Errorf("CIDR with host bits should normalise to a configured network: %v", err)
	}
}

// TestServerEnforcesPolicy proves the wiring: a refused request never reaches
// the executor, and an allowed one does.
func TestServerEnforcesPolicy(t *testing.T) {
	var ran atomic.Int32
	fake := func(_ context.Context, _ string, _ []string, _ string) (string, string, int, error) {
		ran.Add(1)
		return "ok", "", 0, nil
	}
	sock := shortSocket(t)
	srv := NewServerWithPolicy(sock, samplePolicy()).withExec(fake)
	if err := srv.Listen(-1, -1); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); _ = srv.Close() })
	c := NewClient(sock)

	// Out-of-scope: refused before exec.
	if _, _, code, err := c.Run(context.Background(), "route", []string{"-q", "-n", "add", "-inet", "0.0.0.0/0", "-interface", "utun7"}, ""); err == nil {
		t.Error("expected default-route request to be refused")
	} else if code != -1 {
		t.Errorf("refused request code = %d, want -1", code)
	}
	if got := ran.Load(); got != 0 {
		t.Fatalf("executor ran %d times for a refused request, want 0", got)
	}

	// In-scope: reaches exec.
	if _, _, code, err := c.Run(context.Background(), "wireguard-go", []string{"utun7"}, ""); err != nil || code != 0 {
		t.Errorf("allowed request failed: code=%d err=%v", code, err)
	}
	if got := ran.Load(); got != 1 {
		t.Errorf("executor ran %d times, want 1", got)
	}
}

// shortSocket returns a short unix socket path under /tmp (macOS caps paths at
// ~104 bytes, which t.TempDir() can exceed).
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ph")
	if err != nil {
		t.Fatalf("temp socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

func TestLintAnchorRulesetIgnoresCommentsAndBlankLines(t *testing.T) {
	p := samplePolicy()
	rs := "\n# only comments\n\n   \n"
	if err := p.lintAnchorRuleset(rs); err != nil {
		t.Errorf("comment/blank-only ruleset should pass: %v", err)
	}
	if err := p.lintAnchorRuleset("binat on bridge100 from a to any -> b\nnat on bridge100 from any to any -> 1.2.3.4\n"); err == nil {
		t.Error("a nat rule slipped past the linter")
	}
	if !strings.Contains(mustErr(t, func() error { return p.lintAnchorRuleset("pass in all\n") }), "binat/rdr") {
		t.Error("error should explain the binat/rdr restriction")
	}
}

func TestLintAnchorRulesetAllowsLocalVMServiceTarget(t *testing.T) {
	p := samplePolicy()
	rs := "rdr on bridge100 inet proto tcp from any to 10.96.0.1 port 80 -> 192.168.64.5 port 8080\n"
	if err := p.lintAnchorRuleset(rs); err != nil {
		t.Errorf("local VM service target should pass: %v", err)
	}
}

func mustErr(t *testing.T, f func() error) string {
	t.Helper()
	err := f()
	if err == nil {
		t.Fatal("expected an error")
	}
	return err.Error()
}
