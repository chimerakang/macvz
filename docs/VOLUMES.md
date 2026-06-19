# Pod Volumes

MacVz mounts a beta-scoped set of Kubernetes volumes into Pod micro-VMs over
`apple/container`'s VirtioFS shares. This is the operator-facing guide for issue
#26 (**P4 — Hardening & Beta**).

## Supported volume types

| Volume type | Backing | Notes |
| --- | --- | --- |
| `emptyDir` | per-Pod host directory under `node.volumes.root` | ephemeral; removed when the Pod is deleted |
| `emptyDir` with `medium: Memory` | guest tmpfs | no host backing; sized by the guest |
| `hostPath` (directory) | host bind mount over VirtioFS | **disabled by default**; opt in per prefix |

Each container `volumeMount` referencing a supported volume becomes a VirtioFS
share (or tmpfs) at its `mountPath`. `readOnly` mounts are shared read-only into
the guest.

The auto-injected service-account token volume is tolerated (it is not mounted
by MacVz; the guest's own tooling handles it).

## Unsupported (rejected with a clear Failed status)

`configMap`, `secret`, `projected` (other than the SA token), `persistentVolumeClaim`,
`downwardAPI`, `csi`, `nfs`, and any other source are rejected. The Pod gets a
`Failed` status with reason `UnsupportedPodSpec` naming the offending volume.
`subPath`/`subPathExpr` and non-directory `hostPath` types are likewise rejected
for now.

## Configuration

```yaml
node:
  volumes:
    # Host directory backing per-Pod emptyDir volumes (default shown).
    root: /var/lib/macvz/volumes
    # Allowlist of absolute host path prefixes a hostPath volume may use.
    # Empty (default) disables hostPath entirely.
    hostPathAllowedPrefixes:
      - /srv/macvz-shared
```

- `root` and every `hostPathAllowedPrefixes` entry must be absolute paths
  (validated at startup).
- emptyDir volumes are created as `<root>/<podUID>/<volumeName>` before the VM
  starts and removed when the Pod is deleted.

## Security model

- **hostPath is opt-in and prefix-constrained.** With no
  `hostPathAllowedPrefixes`, a Pod cannot bind any host path into a guest. When
  configured, a source is admitted only if — after cleaning, so `..` cannot
  escape — it lies at or below an allowed prefix. Prefix matching is
  path-segment aware, so `/srv` does not admit `/srvother`.
- **Only directory hostPath types** (`""`, `Directory`, `DirectoryOrCreate`) are
  accepted; sockets, char/block devices, and bare files are rejected.
- **emptyDir storage is sandboxed** under `node.volumes.root` and namespaced by
  Pod UID, so Pods cannot read each other's scratch space by path.

## Example

```sh
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: vol-demo
spec:
  restartPolicy: Never
  tolerations:
    - key: virtual-kubelet.io/provider
      operator: Exists
  nodeSelector:
    type: virtual-kubelet
  containers:
    - name: app
      image: alpine
      command: ["sh", "-c", "echo hi > /data/out && sleep 3600"]
      volumeMounts:
        - name: scratch
          mountPath: /data
  volumes:
    - name: scratch
      emptyDir: {}
EOF

kubectl exec vol-demo -- cat /data/out   # -> hi
```

## Known limitations

- Single-container Pods only (the MVP runs one container); volume mounts apply
  to that container.
- No `subPath`, no file-granularity hostPath, no read-only enforcement for
  tmpfs.
- emptyDir size limits (`sizeLimit`) are not enforced.
- Mounted-content volumes (`configMap`/`secret`) are not yet materialized; track
  follow-up work for those.
