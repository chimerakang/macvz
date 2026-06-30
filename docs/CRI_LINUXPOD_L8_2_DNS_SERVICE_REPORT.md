# CRI-L8-2 (#142): k3s DNS and Service discovery on LinuxPod CRI

Parent: #141 (CRI-L8 k3s compatibility hardening). Sibling of the L8 matrix:
#143 image lifecycle (L8-4), #144 reboot recovery (L8-5), #145 volume projection
(L8-3), #146 soak (L8-1), and #147 conformance subset (L8-6).

## Goal

Prove that LinuxPod-backed CRI Pods can use the **normal k3s DNS path** and
Kubernetes Service discovery behavior — CoreDNS reachability, `*.svc` and
`*.svc.cluster.local` resolution, headless-Service Pod A records, and
same-namespace vs other-namespace lookups — not only the direct Pod IP and the
manually probed ClusterIP reachability that CRI-L3/#128 already demonstrated.

The central discipline of this task is **honest layering**: a DNS failure must be
distinguished from a Pod-networking failure and from a Service-routing failure,
never collapsed into one red "it didn't work".

## What landed

- **Fixture** `test/e2e/cri-k3s/fixtures/linuxpod-dns-workload.yaml`: a
  two-container app+late-sidecar Pod (the LinuxPod shared-namespace shape, like
  `linuxpod-workload.yaml`), a ClusterIP Service, a **headless** Service
  (`clusterIP: None`), and a ConfigMap. The sidecar does a boot-time DNS
  self-resolution of its own `*.svc` name and records a proof file into the
  shared `emptyDir`. Other-namespace lookups reuse the always-present
  `kubernetes.default.svc` and `kube-dns.kube-system.svc`, so the fixture stays
  lean.
- **Harness** `test/e2e/cri-k3s/linuxpod-dns.sh` (`make cri-linuxpod-dns`): the
  DNS/Service-discovery sibling of `linuxpod-inloop.sh`. Plan-only and exit 0
  without `MACVZ_INTEGRATION=1` + a reachable `KUBECONFIG`, so it is safe under
  `bash -n` / `shellcheck` / CI. Phases: preflight, route-before, deploy,
  scheduling, backend-evidence (honesty gate), resolver-config, coredns-reach,
  nxdomain-control, svc-discovery (short + FQDN), headless, cross-namespace,
  dns-vs-route, sidecar-dns, then re-runs the DNS core after **rollout**,
  **macvz-cri restart**, **helper restart**, and **netd reload**, then cleanup
  and route-after.

### How failures are distinguished (the acceptance's core requirement)

- **resolver-config** — does the Pod even have an injected `/etc/resolv.conf`
  with a cluster nameserver + the expected `<ns>.svc`, `svc.<domain>`, `<domain>`
  search list? A missing resolver is a **DNS-config-injection gap**, classified
  apart from any resolution result.
- **coredns-reach** — a known-good name (`kubernetes.default.svc`) must resolve
  to the kubernetes Service ClusterIP (CoreDNS up + answering).
- **nxdomain-control** — a known-bad name must come back as an **authoritative
  NXDOMAIN**, not a timeout; this proves CoreDNS is answering rather than being
  unreachable.
- **dns-vs-route** — resolve a Service by name, then curl it **both** by name and
  by the resolved ClusterIP: by-IP-ok / by-name-fail is an isolated DNS-layer
  fault, by-IP-fail is a Service-routing fault independent of DNS.
- The classifier (`dns_failure_kind`) tags every empty answer as `no-tool`
  (image lacks a resolver applet), `unreachable` (query hung / DNS server not
  reached / exec transport timed out), or `nxdomain` (record absent but CoreDNS
  answered) — so an operator report always names the real layer.

### Honesty gate

Inherited from `linuxpod-inloop.sh` (#130): the DNS checks run on either
backend, but the LinuxPod-specific framing is only claimed when
`MACVZ_LINUXPOD_BACKEND_EVIDENCE_CMD` proves the Pod is a genuine, non-simulated
LinuxPod backend. Absent that proof the suite reports the DNS result against the
apple/container path and says so loudly.

### Validation

`bash -n` and `shellcheck -S warning` clean; `kubectl apply --dry-run=client`
accepts the fixture; plan-only mode prints the runbook and exits 0. The shipped
Virtual Kubelet and apple/container paths are untouched.

## Live findings (2026-06-30, `kind-macvz61` + `test@192.168.1.122`, node `macvz-b-cri`)

The hardened harness was run live against the real LinuxPod CRI node (genuine
non-simulated LinuxPod backend, protocol 6). The canonical run
(`/tmp/macvz-live-142-dns-run2/run`) ended **0 failures / 26 skips**: the honesty
gate passed, scheduling was clean, the `macvz-cri` restart preserved the Pod UID,
the helper restart recovered the Pod to Running, the `macvz-netd` reload left the
default route unchanged, and the run-wide route audit confirmed `192.168.1.1` via
`en0` before and after. Every DNS *resolution* check was uniformly blocked on a
single precise finding (below), reported as a loud **blocked-skip**, not a faked
pass — so the suite flips to hard PASS/FAIL the moment DNS injection lands.

The node is a genuine LinuxPod serving path: a **two-container** Pod
(`dnsPolicy: ClusterFirst`) deployed and reached rollout-available — a shape the
apple/container path excludes (#82/#86) — confirming the Pod is LinuxPod-backed.
The finding below was independently reproduced on **two** fresh LinuxPod Pods in
isolated namespaces during follow-up probing.

**The precise blocker — DNS config is not injected into the LinuxPod guest.**
On two independent freshly-Running LinuxPod Pods, the container had **no
`/etc/resolv.conf`**:

```
$ kubectl exec <pod> -c app -- busybox cat /etc/resolv.conf
cat: can't open '/etc/resolv.conf': No such file or directory
```

despite the Pod's `dnsPolicy` being `ClusterFirst`. In CRI mode the kubelet
hands the resolver config to the runtime to materialize inside the container;
the LinuxPod backend does not yet plumb it into the guest rootfs. Consequently
**in-Pod cluster DNS and name-based Service discovery cannot work yet** on the
LinuxPod CRI path. This is a **DNS-config-injection gap** (overlapping the #128
networking surface), *not* a missing DNS record and *not* a CoreDNS outage — the
exact distinction this task exists to make.

**Secondary findings:**

- **Resolver tool packaging.** The vminitd BusyBox ships the `nslookup` applet in
  the multi-call binary but does **not** symlink `/bin/nslookup`, so a bare
  `nslookup` is "not found" while `busybox nslookup` works. The harness and the
  fixture's sidecar were hardened to prefer the bare command and fall back to
  `busybox nslookup`, and the classifier reports a genuinely missing applet as
  `no-tool` (a fixture/image issue) rather than a DNS failure.
- **Exec-path flakiness during probing.** Repeated `kubectl exec` of blocking
  DNS queries returned `supervisor … .sock: read timeout` from the adapter,
  while fast non-blocking commands (e.g. `cat /etc/resolv.conf`) succeeded. This
  is consistent with there being no working resolver (the blocking query never
  gets an answer and hits the exec read deadline). It also prevented confirming
  the explicit-server CoreDNS *routing* question (whether the Pod network can
  reach `kube-dns` at all, independent of the missing `resolv.conf`); the
  harness classifies these exec-transport timeouts as `unreachable` distinctly
  and they are recorded as a co-blocker for the live pass.

**Host default route** was never mutated by any of this work
(`192.168.1.1` via `en0` throughout); all probing was read-only over the cluster
API and isolated test namespaces, which were cleaned up.

## Outcome

`linuxpodDNSDiscoveryBlocked` — the repeatable, gated harness and fixture are
landed and the live run **precisely identifies the blocker**: DNS/Service
discovery by name is not yet functional on the LinuxPod CRI path because the
backend does not inject the kubelet-provided `/etc/resolv.conf` into the guest.
This mirrors the #119 `kubeletHandoffSmokeBlocked` discipline — the code/harness
blocker is removed and the precise runtime gap is named, rather than a false
pass.

### Remaining work to close #142

1. **Inject the cluster resolver** into the LinuxPod guest rootfs at container
   create (the kubelet-provided `resolv.conf` from the CRI `DNSConfig`), so
   `/etc/resolv.conf` carries the cluster nameserver + search list. This is the
   blocking change; it most naturally lands alongside the #128 networking path.
2. **Stabilize the LinuxPod exec path** under repeated/blocking queries so the
   live suite can run the full resolution matrix (coredns-reach, nxdomain-control,
   svc-discovery, headless, cross-namespace, dns-vs-route) and the
   rollout/cri/helper/netd re-checks to a green PASS.
3. **Re-run** `make cri-linuxpod-dns` live on `test@192.168.1.122` with the
   backend-evidence and churn hooks set, and update this report + the
   `docs/MASTER_TASKS.md` row to ✅ with the passing evidence.

## Non-goals honored

No custom scheduler / API server / control plane; no change to the shipped
Virtual Kubelet or apple/container path; no mutation of the host default route or
pf policy; no production support claim — this is experimental k3s DNS validation
on the LinuxPod CRI path.
