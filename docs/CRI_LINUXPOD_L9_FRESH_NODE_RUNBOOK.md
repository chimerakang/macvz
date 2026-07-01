# CRI-L9-5 — Fresh-node LinuxPod k3s install rehearsal (#153)

Status: **runbook + hermetic rehearsal landed; live fresh-node evidence pending.**

This runbook proves the CRI node operator path works from a clean (or cleaned)
node using only documented commands — not the long-lived development directory.
The hermetic half runs anywhere (`make cri-linuxpod-install-rehearsal`); the
live half targets `test@192.168.1.122` and must be executed by an operator with
the node reachable.

Companion docs: [CRI_NODE_OPERATIONS.md](CRI_NODE_OPERATIONS.md) (wiring
reference, k3s flags, failure table), [PRIVILEGED_NETWORKING.md](PRIVILEGED_NETWORKING.md)
(macvz-netd), and the installer's own help
(`scripts/macvz-cri-linuxpod-install.sh --help`).

## 0. Hermetic rehearsal (no root, no hardware)

```sh
make cri-linuxpod-install-rehearsal
```

Exercises install → idempotent reinstall → status → upgrade → rollback →
clean (dry-run + `--force`) → uninstall `--purge` against a temp prefix with
`launchctl` stubbed, asserting the versioned pair layout, rendered
ProgramArguments, previous/rollback bookkeeping, and audited cleanup. This must
pass before any live run.

## 1. Route guard (before anything)

```sh
route -n get default | awk '/gateway|interface/'   # record; must be identical at the end
```

Nothing in this runbook may change the host default route. Re-run and compare
after every numbered step if in doubt, and always at the end.

## 2. Clean state

On a previously-used node, remove the prior LinuxPod CRI services and state:

```sh
scripts/macvz-cri-linuxpod-install.sh uninstall --purge
scripts/macvz-cri-linuxpod-install.sh clean          # audit whatever survives
scripts/macvz-cri-linuxpod-install.sh clean --force  # then delete it
```

Leave macvz-netd and apple/container installed — they are node prerequisites,
not per-install state. Verify no stale processes:
`pgrep -fl 'macvz-cri|linuxpod-helper' || echo clean`.

## 3. Install order (documented order, one command each)

1. **apple/container** system service running as the operator user
   (`container system status`).
2. **macvz-netd** root LaunchDaemon (once per node):
   `sudo macvz-netd install --socket /var/run/macvz-netd.sock --config /etc/macvz/config.yaml`
   — see PRIVILEGED_NETWORKING.md, including narrowing `vmNetCIDRs`.
3. **linuxpod-helper + macvz-cri pair** via the installer. Build and sign first:

   ```sh
   make cri
   (cd test/e2e/cri-linuxpod && swift build)
   cp test/e2e/cri-linuxpod/.build/*/debug/linuxpod-helper bin/
   codesign -f -s - --entitlements test/e2e/cri-linuxpod/linuxpod-helper.entitlements bin/linuxpod-helper
   ```

   Then install with the node's wiring (values below are the live-lab shape;
   adjust per node):

   ```sh
   scripts/macvz-cri-linuxpod-install.sh install --from bin \
     --kernel "$HOME/containerization/bin/vmlinux-arm64" \
     --containerization-root "$HOME/containerization/bin" \
     --vmnet \
     --kubelet-pods-dir "$HOME/kubelet-root/pods" \
     --volume-host-path-allowed "$HOME/kubelet-root" \
     --pod-cidr 10.244.102.0/24 \
     --pod-network-interface bridge100 \
     --pod-network-ingress-interface en0 \
     --pod-network-enable-forwarding \
     --streaming-addr 192.168.1.122:0
   ```

   `status` must show both LaunchAgents loaded, both sockets live, and the full
   auditable argument list. `preflight` must pass. A protocol mismatch between
   the paired binaries fails loudly in `log/macvz-cri.err.log` before serving.
4. **k3s agent** pointed at the CRI socket (flags in CRI_NODE_OPERATIONS.md §5),
   or the kind + ssh-forward test topology (appendix there).

## 4. Smoke (minimal Deployment + Service + kubelet surfaces)

With `KUBECONFIG` set to the cluster:

```sh
MACVZ_INTEGRATION=1 make cri-linuxpod-inloop   # automated version of this smoke
```

or manually: apply `test/e2e/cri-k3s/fixtures/linuxpod-workload.yaml`, wait
Ready, then verify `kubectl logs`, `kubectl exec`, `kubectl port-forward`, and
Service reachability. Every Pod must prove `simulated=false` (helper Ping) —
never accept a silently simulated pass.

## 5. Diagnostics + evidence collection

```sh
scripts/macvz-cri-linuxpod-install.sh status
~/.macvz/cri-linuxpod/current/macvz-cri --support-bundle \
  --linuxpod-helper-socket ~/.macvz/cri-linuxpod/linuxpod-helper.sock \
  --linuxpod-helper-work-dir ~/.macvz/cri-linuxpod/helper-work \
  --state-dir ~/.macvz/cri-linuxpod/state
```

Record: route guard before/after (identical), installer status output, smoke
results, and the bundle path. On failure, the CRI_NODE_OPERATIONS.md failure
table maps the symptom to the misconfigured service/socket.

## 6. Return to known-good reusable state

```sh
scripts/macvz-cri-linuxpod-install.sh uninstall           # keep versions/state for reuse
# or, to fully reset:
scripts/macvz-cri-linuxpod-install.sh uninstall --purge
```

Re-check the route guard one final time.

## Live evidence

| Date | Node | Result | Evidence |
| --- | --- | --- | --- |
| _pending_ | test@192.168.1.122 | — | — |
