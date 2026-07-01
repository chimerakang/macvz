# Diagnostic Bundle

`macvz-kubelet bundle` produces a redacted, self-contained snapshot of a MacVz
node's state for support requests and bug reports (#59). It gathers the context
needed to debug common runtime, control-plane, and data-plane failures, strips
the secrets it can recognise, and packages the result into a timestamped
directory and `tar.gz` you can attach to a GitHub issue.

## Usage

```sh
# Collect a bundle into the OS temp dir, as a tar.gz (the default).
macvz-kubelet bundle --config /etc/macvz/config.yaml

# Choose the output directory and keep it unpacked (no archive).
macvz-kubelet bundle --config /etc/macvz/config.yaml --out ./support --no-archive

# Include extra log files (e.g. the kubelet or macvz-netd log captured by launchd).
macvz-kubelet bundle --config /etc/macvz/config.yaml \
  --log-file /usr/local/var/log/macvz-kubelet.log,/var/log/macvz-netd.log
```

Flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--config` | _(none)_ | Path to the macvz-kubelet config to summarise. |
| `--out` | OS temp dir | Directory the bundle is written into. |
| `--no-archive` | `false` | Leave the bundle as a directory; do not create a `tar.gz`. |
| `--log-file` | _(none)_ | Comma-separated extra log files to include. |
| `--events` | `50` | Maximum recent Kubernetes events to include. |

Some sources need root (`wg show`, `pfctl -a … -s all`). Run with `sudo` to
capture them; without it, those sections record the permission error as context
rather than failing the bundle.

## What it collects

The bundle is scoped to what the config enables (mesh/podNetwork/helper sections
are skipped when those features are off):

| Path | Source |
| --- | --- |
| `metadata.txt` | Version, node name, host, internal IP, feature toggles. |
| `config/config-loaded.yaml` | Parsed config with defaults applied. |
| `config/config-raw.yaml` | The raw config file as written. |
| `health/diagnostics.txt` | Live node health report (control-plane / runtime / data-plane), the same model as `/healthz/diagnostics` (#56). |
| `kubernetes/node.yaml` | The node object from the API server. |
| `kubernetes/events.txt` | Recent cluster events. |
| `runtime/system-status.txt` | `container system status`. |
| `runtime/containers.txt` | `container list --all`. |
| `runtime/images.txt` | `container image ls`. |
| `network/helper-status.json` | Privileged helper (`macvz-netd`) self-report. |
| `network/routes.txt` | Host routing table (`netstat -rn`). |
| `network/ip-forwarding.txt` | `net.inet.ip.forwarding` state. |
| `network/mesh-interface.txt` | `ifconfig <mesh-iface>`. |
| `network/wireguard.txt` | `wg show <mesh-iface>` (needs root). |
| `network/pf-anchor-rules.txt` | `pfctl -a <anchor> -s all` (needs root). |
| `logs/*` | Any files passed via `--log-file`. |
| `manifest.txt` | Index of every file, byte counts, and per-source errors. |

A source that fails (unreachable API server, missing tool, permission denied)
writes a `<name>.error` sidecar and is noted in `manifest.txt`; it never aborts
the rest of the bundle, so a broken subsystem — the thing you are usually
debugging — still produces a usable bundle.

## Redaction

Every byte a source produces passes through a redactor **before** it is written
to disk, so a new collector cannot accidentally leak a credential. The redactor
replaces with `[REDACTED]`:

- PEM private-key blocks (`-----BEGIN … PRIVATE KEY-----`, including OpenSSH);
- WireGuard `PrivateKey`/`PresharedKey` values (public keys are kept);
- JSON Web Tokens and `Bearer` tokens (e.g. ServiceAccount tokens);
- the values of a curated set of sensitive keys — `password`, `token`,
  `secret`, `*_key`/`*-key-data`, `apikey`, `authorization`, `credentials`, and
  similar — in YAML, JSON, kubeconfig, and env output.

Public material needed for debugging is deliberately **kept**: certificates,
public keys, CA data, server URLs, interface names, and CIDRs.

Redaction is best-effort and is the security boundary of this command. Always
skim the bundle before sharing it; if you spot a secret the redactor missed,
report it so a pattern can be added.

## macvz-cri support bundle (CRI/LinuxPod path)

`macvz-cri --support-bundle` is the CRI-node counterpart of `macvz-kubelet
bundle` (CRI-L9-3, #151). It collects the k3s-compatible CRI adapter's state —
including the experimental LinuxPod backend's helper handshake and journals —
prints the output path, and exits without serving.

```sh
# Bundle for a LinuxPod-backed CRI node, including the helper's journals and
# tails of the adapter/helper logs.
macvz-cri --support-bundle \
  --state-dir ~/.macvz/cri/sandboxes \
  --linuxpod-helper-socket /tmp/linuxpod-helper.sock \
  --linuxpod-helper-work-dir ~/.macvz/linuxpod-helper \
  --pod-network-helper-socket /var/run/macvz-netd.sock \
  --bundle-log-file /usr/local/var/log/macvz-cri.log \
  --bundle-log-file /usr/local/var/log/linuxpod-helper.log
```

Flags (in addition to the serving flags it reuses — `--listen`, `--state-dir`,
`--streaming-addr`, `--linuxpod-helper-socket`, `--pod-network-helper-socket`):

| Flag | Default | Meaning |
| --- | --- | --- |
| `--support-bundle` | `false` | Collect the bundle, print its path, and exit. |
| `--bundle-out` | `./macvz-cri-bundle-<timestamp>` | Directory the bundle is written into. |
| `--bundle-log-file` | _(none)_ | Extra log file whose last ~500 KB is included (repeatable). |
| `--no-archive` | `false` | Leave the bundle as a directory; do not create a `tar.gz`. |
| `--linuxpod-helper-work-dir` | _(none)_ | LinuxPod helper `--work-dir` to collect journals and a residue listing from. |

What it collects:

| Path | Source |
| --- | --- |
| `meta/version.txt` | macvz-cri version, os/arch, host, timestamp. |
| `meta/args.txt` | The (redacted) command line. |
| `linuxpod/helper-info.json` | Helper handshake/Ping: protocol version, capabilities, simulated flag, adoption counts (when `--linuxpod-helper-socket` is set). |
| `linuxpod/residual-state.json` | The `--diagnose-linuxpod` residual-state report over the persisted stores. |
| `linuxpod/supervisor-journal.json` | The helper's router journal from `--linuxpod-helper-work-dir`. |
| `linuxpod/adoption-journal.json` | The helper's backend adoption journal. |
| `linuxpod/helper-workdir.txt` | Names+sizes listing of the helper work dir (exposes leftover `sup-*` residue). |
| `state/sandboxes.txt` | Summary of persisted sandbox records (IDs, identity, state, timestamps, Pod IP). |
| `state/containers.txt` | Summary of persisted container records (IDs, image, state, exit info). |
| `network/netd-status.json` | `macvz-netd` self-report over `--pod-network-helper-socket`. |
| `net/sockets.txt` | Existence/mode/age of the CRI socket, helper sockets, and the streaming addr config. |
| `logs/*` | Tail (last ~500 KB) of each `--bundle-log-file`. |
| `manifest.txt` | Index of every file, byte counts, and per-source errors. |

The bundle is fail-soft: a source that fails (unreachable helper, missing
journal) writes a `<name>.error` sidecar and is noted in `manifest.txt`; only a
bundle that cannot be written at all fails the command. Every byte passes the
same redactor as the `macvz-kubelet` bundle before it touches disk (PEM private
keys, WireGuard keys, JWT/bearer tokens, sensitive `key: value` pairs →
`[REDACTED]`), so the result is safe to attach to a GitHub issue — but as with
the VK bundle, redaction is best-effort: skim it before sharing.
