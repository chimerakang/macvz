# CRI-P8 k3s compatibility suite

Experimental MacVz CRI feasibility track (`develop`), issue #80. This directory
documents how a k3s/kubelet points at the experimental `macvz-cri` adapter and
runs a realistic `crictl` compatibility, restart, and soak suite. It is **not**
the shipped Virtual Kubelet path (`test/e2e/e2e.sh`) and must not gate the VK
release.

## Layout

- `run.sh` — gated compatibility suite: adapter handshake, single-container Pod
  lifecycle, logs, exec, projected config mount, unsupported-shape diagnostic,
  adapter restart recovery, and cleanup verification.
- `soak.sh` — gated bounded soak: repeated create/delete cycles sampling adapter
  RSS and orphan counts, with leak/orphan guards.

Both are gated by `MACVZ_INTEGRATION=1`; without it they print their plan and
exit 0, so they are safe in `go test`-style CI.

## Quick start

```sh
make cri
MACVZ_INTEGRATION=1 ./test/e2e/cri-k3s/run.sh
MACVZ_INTEGRATION=1 ./test/e2e/cri-k3s/soak.sh
```

These self-manage a throwaway adapter on a temp socket — no cluster required.
They exercise the CRI contract a kubelet drives, including the explicit
`PullImage` step before `CreateContainer`.

## Pointing k3s at macvz-cri

apple/container is a per-user service and refuses to run as root, so the adapter
runs as the operator (a per-user LaunchAgent via `scripts/macvz-cri-install.sh`),
and k3s must be configured to use that external CRI endpoint rather than its
bundled containerd.

1. Install the adapter as a managed per-user service:

   ```sh
   make cri
   ./scripts/macvz-cri-install.sh install --from ./bin \
     --socket "$HOME/.macvz/cri/macvz-cri.sock" \
     --state-dir "$HOME/.macvz/cri/state"
   ./scripts/macvz-cri-install.sh status
   ```

   For Pod networking, pass adapter flags through `MACVZ_CRI_EXTRA`, e.g.
   `MACVZ_CRI_EXTRA="--pod-cidr 10.42.0.0/24 --pod-network-interface bridge100"`.
   If an argument contains spaces, put one argument per line in a file and set
   `MACVZ_CRI_EXTRA_ARGS_FILE=/path/to/args`.

2. Start k3s (agent) against the external endpoint:

   ```sh
   k3s agent \
     --container-runtime-endpoint "unix://$HOME/.macvz/cri/macvz-cri.sock" \
     --server https://<k3s-server>:6443 --token <node-token>
   ```

   Equivalent `config.yaml`:

   ```yaml
   container-runtime-endpoint: "unix:///Users/<you>/.macvz/cri/macvz-cri.sock"
   ```

   Startup ordering: apple/container and (optionally) `macvz-netd` must be up
   before the adapter, and the adapter before k3s. The LaunchAgent's `KeepAlive`
   plus the adapter's restart recovery make a relaunch safe.

3. Run the CRI suite against the managed adapter (do not let it manage its own):

   ```sh
   MACVZ_INTEGRATION=1 MACVZ_CRI_MANAGE=0 \
     MACVZ_CRI_SOCKET="$HOME/.macvz/cri/macvz-cri.sock" \
	    ./test/e2e/cri-k3s/run.sh
	   ```

   A full `kubectl` fixture deployment and Service reachability smoke against a
   live k3s cluster are intentionally left for CRI-P9 go/no-go evidence.

## Cleanup

```sh
./scripts/macvz-cri-install.sh uninstall --purge
```

Uninstall removes the LaunchAgent, binary, and socket; `--purge` also deletes the
state dir. The suite asserts no stale socket, workload, or sandbox remains.

## Known limitations

See `docs/CRI_FEASIBILITY.md` (CRI-P8) for the precise list — multi-container
Pods, host-namespace Pods (rejected with a clear diagnostic), and the kubelet's
ownership of probes/projected volumes in CRI mode.
