# Workload lifecycle and restart semantics

MacVz runs each Pod as a single Linux micro-VM through `apple/container`. This
document defines how container exit and Kubernetes `restartPolicy` map onto that
one-container-per-micro-VM model (#45), so Deployment- and ReplicaSet-managed
Pods behave the way Kubernetes operators expect.

## Restart policy

The provider honors all three Kubernetes restart policies. An unset policy
defaults to `Always`, matching the API server, which populates the field before
a Pod ever reaches the node.

| `restartPolicy` | On clean exit (code 0) | On failure (non-zero) |
| --------------- | ---------------------- | --------------------- |
| `Always`        | restart                | restart               |
| `OnFailure`     | Pod `Succeeded`        | restart               |
| `Never`         | Pod `Succeeded`        | Pod `Failed`          |

Controllers set the policy in their Pod template: Deployments and ReplicaSets
use `Always`, Jobs use `OnFailure` or `Never`. Because the provider now accepts
these, a controller-created Pod is admitted whenever the rest of its spec is
compatible.

## What "restart" means for a micro-VM

A container does not restart in place. When a workload exits under a policy that
calls for a restart, the provider:

1. waits out a backoff (10s, doubling on repeated exits, capped at 5m — the
   kubelet's CrashLoopBackOff behavior);
2. tears down the exited micro-VM and its stale Pod-network mapping;
3. creates and starts a **fresh** micro-VM from the same container spec (the
   image is already local, so no re-pull); and
4. re-attaches the new VM to the Pod network path. The Pod keeps its stable Pod
   IP; only the internal host-only VM address changes.

Each restart increments the container's `RestartCount`, visible in
`kubectl get pod` and `kubectl describe pod`.

## Status surfaced to Kubernetes

- While a restart is pending or in flight, the container reports a `Waiting`
  state with reason `CrashLoopBackOff`, and the Pod is not `Failed`.
- Once the new micro-VM is running, the container returns to `Running` with the
  incremented `RestartCount`.
- Under `Never`, a terminated container stays terminal: exit 0 → Pod
  `Succeeded`, non-zero → Pod `Failed`.
- A Pod spec the node cannot run at all (e.g. `hostNetwork`, multi-container)
  still fails fast with a sticky `Failed` status and an actionable message,
  regardless of restart policy.

## Kubelet restart recovery (#66)

`apple/container` micro-VMs outlive the `macvz-kubelet` process: restarting (or
upgrading) the kubelet does not stop running workloads. The provider keeps its
Pod→workload map only in memory, so on restart it must re-attach to the VMs that
are still running rather than rebuild them.

Recovery hangs off two facts that make it deterministic, not stateful:

1. A workload's ID is a pure function of the Pod's identity
   (`WorkloadID(namespace, name, container)`), so the VM backing a given Pod can
   be found again by name without any persisted bookkeeping.
2. Each Pod's IP is restored *before* the Pod controller runs by
   `RecoverAllocations`, which reserves the `Status.PodIP` already recorded on
   the API server. The recovered Pod therefore keeps the same IP it advertised
   in its EndpointSlices.

When Virtual Kubelet replays its Pods after a restart, `CreatePod` probes the
runtime for the Pod's deterministic workload ID. If a micro-VM is already there,
the provider **adopts** it: it skips the image pull and create, records the
existing workload, re-attaches the Pod network path, and resumes probing. A VM
already in `Running` is left untouched; a VM stranded in `Created` because the
previous kubelet died between create and start is started in place. Only a Pod
with no surviving VM takes the normal pull/create/start path.

Adopting instead of recreating matters for more than speed: a re-pull could now
fail (for example, a private-registry `imagePullSecret` rotated since the VM
started), which would strand a perfectly healthy container in a failing Pod. The
driver's `Create` is idempotent as a backstop, so even if the probe misses on a
transient runtime error, recovery can never duplicate a VM. If the network
re-attach fails mid-recovery, the adopted VM is left running (not destroyed) so
the next reconcile can retry.

Restart counts are not persisted; an adopted container resumes from
`RestartCount` 0. The container itself is the same process that was running
before the kubelet restarted.

## Validation

- Provider unit tests in `pkg/provider/restart_test.go` cover the policy
  decision table, backoff growth/cap, an `Always` restart of an exited workload,
  `OnFailure` skipping a clean exit, `Never` staying terminal, and the
  `CrashLoopBackOff` waiting state.
- Kubelet restart recovery (#66) is covered in `pkg/provider/pod_test.go`:
  adopting an already-running micro-VM without pull/create/start, and adoption
  succeeding even when the image can no longer be pulled (rotated pull Secret),
  starting a recovered VM that was left in `Created`, and handing a recovered
  `Stopped` VM to the normal restart loop.
- Manual smoke: `kubectl create deployment web --image=nginx` should roll out to
  `Running` on a MacVz node; deleting the backing micro-VM, or running an image
  that exits, should show `RestartCount` climb while the Deployment self-heals.
- Manual restart smoke: with a Pod `Running`, restart `macvz-kubelet`; the Pod
  should return to `Running` with the same Pod IP and **no** new image pull or
  micro-VM in `container ls`.

## ConfigMaps (#46)

Pods consume ConfigMaps the same two ways as in stock Kubernetes, resolved from
the node's ConfigMap informer cache.

### Environment variables

- `env[].valueFrom.configMapKeyRef` injects a single key as one variable.
- `envFrom[].configMapRef` injects every key, with an optional `prefix`. Keys
  that are not valid environment variable names are skipped (as the kubelet
  does), so the rest of the Pod still starts.
- Precedence follows Kubernetes: `envFrom` values are the base, and explicit
  `env` entries (literals and `*KeyRef`) override them. Literal `$(VAR)`
  references are expanded against variables defined earlier.

### Volumes

A `configMap` volume is materialized into a per-Pod host directory and bind-
mounted **read-only** into the guest. It needs `node.volumes.root` configured
(the same backing root as `emptyDir`). Supported projection options:

- whole-ConfigMap projection (one file per key, including `binaryData`),
- `items` (a subset of keys, each with a target path that may include
  subdirectories, and an optional per-file mode),
- `defaultMode` for files without an explicit mode.

The directory is created on `CreatePod` and removed with the Pod, alongside its
`emptyDir` storage.

### Optional, missing, and updates

- **Optional** (`optional: true`): an absent ConfigMap or key is skipped — the
  env var is omitted, or the volume mounts as an empty directory.
- **Missing required**: an absent ConfigMap, or a missing required key, is **not
  terminal**. `CreatePod` returns a retryable error, so Virtual Kubelet leaves
  the Pod `Pending` with a `ProviderFailed` reason naming the missing object and
  retries; the Pod starts once the ConfigMap appears.
- **Updates**: ConfigMap data is read once, when the Pod's micro-VM is created.
  Live in-place updates to a running Pod's env or mounted files are **not**
  propagated — recreate the Pod (e.g. roll the Deployment) to pick up changes.
  Secret-backed env/volumes are tracked separately under #47.

### Validation

- `pkg/provider/configmap_test.go` covers `configMapKeyRef`, `envFrom` prefix and
  precedence, invalid-name skipping, optional-absent, missing-required (Pending),
  volume file materialization, `items`, and the CreatePod retry path.
- Manual smoke: create a ConfigMap, reference it from a Deployment via both
  `envFrom` and a `configMap` volume, and confirm the values land in the guest's
  environment and under the mount path.

## Secrets (#47)

Pods consume Secrets the same two ways as ConfigMaps, resolved from the node's
Secret informer cache (the same cache that serves `imagePullSecrets`). Secret
values are never written to logs, diagnostics, or error messages — failures name
only the Secret and key.

### Environment variables

- `env[].valueFrom.secretKeyRef` injects a single key as one variable.
- `envFrom[].secretRef` injects every key, with an optional `prefix`. Keys that
  are not valid environment variable names are skipped, as the kubelet does.
- Precedence is unified with ConfigMaps and the Downward API in one resolver:
  `envFrom` values (ConfigMap and Secret) are the base, and explicit `env`
  entries override them.

### Volumes

A `secret` volume is materialized into a per-Pod host directory and bind-mounted
**read-only** into the guest (Kubernetes always mounts Secret volumes read-only).
It needs `node.volumes.root` configured (the same backing root as `emptyDir` and
`configMap`). Supported projection options:

- whole-Secret projection (one file per key),
- `items` (a subset of keys, each with a target path that may include
  subdirectories, and an optional per-file mode),
- `defaultMode` for files without an explicit mode (default `0644`).

The directory is created on `CreatePod` and removed with the Pod, alongside its
`emptyDir` storage.

### Optional, missing, and updates

- **Optional** (`optional: true`): an absent Secret or key is skipped — the env
  var is omitted, or the volume mounts as an empty directory.
- **Missing required**: an absent Secret, or a missing required key, is **not
  terminal**. `CreatePod` returns a retryable error (`errSecretUnavailable`), so
  Virtual Kubelet leaves the Pod `Pending` and retries; the Pod starts once the
  Secret appears.
- **Updates**: Secret data is read once, when the Pod's micro-VM is created.
  Live in-place updates to a running Pod are **not** propagated — recreate the
  Pod to pick up changes.

### Validation

- `pkg/provider/secrets_test.go` covers `secretKeyRef`, `envFrom` prefix and
  precedence, invalid-name skipping, optional-absent, missing-required (Pending),
  read-only volume file materialization, `items` with custom modes, the empty-dir
  optional case, no-value-leak on a missing key, and the CreatePod retry path.
- Manual smoke: create a Secret, reference it from a Deployment via both
  `secretKeyRef`/`envFrom` and a `secret` volume, and confirm the values land in
  the guest's environment and under the mount path with the expected file modes.

## imagePullSecrets and private registries (#49)

A Pod can pull a private image by naming a docker pull Secret in
`imagePullSecrets`. Before pulling, the provider resolves the credential for the
image's registry and hands it to the runtime.

### Resolution

- Both `kubernetes.io/dockerconfigjson` (`.dockerconfigjson`, with an `auths`
  map) and the legacy `kubernetes.io/dockercfg` (`.dockercfg`) Secrets are
  accepted. The credential is taken from the entry's `username`/`password`, or
  from a base64 `auth` field in `user:password` form.
- The image's registry host is matched against each entry's key (scheme and path
  are stripped, so `https://index.docker.io/v1/` matches a Docker Hub image). An
  image with no registry prefix (e.g. `nginx`, `library/nginx`) resolves to
  Docker Hub. The Pod's pull secrets are tried in order; the first match wins.
- If no named secret holds a credential for the registry, the image is pulled
  anonymously — matching Kubernetes.

### How credentials reach the runtime

`apple/container` has no inline pull credentials, so the driver runs
`registry login <server> --username <u> --password-stdin`, then `image pull`,
then `registry logout <server>`. The password is passed on stdin — never in
process arguments or logs — and the logout drops the credential from the runtime
store after the pull. The login/pull/logout sequence is serialized per registry
server. No credential is ever written to a repo-controlled path.

### Failure behavior

- A named pull Secret that is missing is transient: the Pod stays Pending with an
  actionable message and retries, so it self-heals once the Secret appears.
- A malformed pull Secret, or a matching registry entry with no usable
  credential, surfaces a clear error (and is retried). Wrong credentials fail at
  pull time with the registry's own error.

### Validation

- `pkg/provider/pullsecrets_test.go` covers registry-host extraction, key
  normalization, dockerconfigjson and legacy dockercfg parsing, the `auth`
  base64 form, Docker Hub aliasing, first-match ordering, anonymous fall-back,
  and the missing/malformed/unusable error paths.
- `pkg/provider/pod_test.go` asserts the resolved credential flows through
  `CreatePod` into the pull, that a Pod with no pull secret pulls anonymously,
  and that a missing pull Secret is transient (runtime untouched, Pod not
  tracked).
- `pkg/runtime/container/driver_test.go` asserts the login/pull/logout sequence,
  the password over stdin (never in argv), and that a failed login aborts the
  pull.
- Manual smoke: push an arm64 image to a private registry, create a
  `dockerconfigjson` Secret, reference it from a Pod via `imagePullSecrets`, and
  confirm the Pod reaches Running.

## ServiceAccount tokens and in-cluster API access (#51)

Kubernetes auto-injects a projected `kube-api-access-*` volume into every Pod so
in-cluster clients can reach the API server. MacVz materializes that volume as
files under the standard mount path, giving compatible workloads normal,
RBAC-bound API access.

### What is materialized

The projected volume's three sources are written under
`/var/run/secrets/kubernetes.io/serviceaccount` (the mount path the Pod
declares), at the volume's `defaultMode` (0644 unless set):

- `token` — a bound service-account token, minted through the API server's
  `TokenRequest` subresource (`ServiceAccounts(namespace).CreateToken`) for the
  Pod's effective ServiceAccount (`default` when the Pod names none). The token
  is bound to the Pod via a `BoundObjectRef`, so it is invalidated when the Pod
  is deleted and cannot outlive it.
- `ca.crt` — the cluster CA, projected from the `kube-root-ca.crt` ConfigMap in
  the Pod's namespace (the same ConfigMap resolver used by #46).
- `namespace` — the Pod's namespace, from the projected downward-API source.

Projected `configMap`, `secret`, and `downwardAPI` (`fieldRef`) sources are
honored generally, not only for the auto-injected volume.

### Token lifetime

The token is requested honoring the projection's `expirationSeconds` (default
1h, floored at the apiserver minimum of 10m) and materialized **once**, when the
Pod's micro-VM is created. MacVz does **not** rotate the on-disk file while the
micro-VM runs — the volume is bound read-only into the guest. A workload that
must outlive the token's expiry should tolerate re-issue on Pod recreation;
controller-managed Pods (#45) get a fresh token each time their micro-VM is
recreated. This is the documented lifetime behavior for the beta.

### Enablement and failure behavior

- Token materialization requires a token issuer wired to the live clientset.
  The kubelet wires it automatically; on a node without one the auto-injected
  volume is tolerated but not mounted, so the Pod simply gets no credentials
  (prior behavior).
- A failed `TokenRequest` (e.g. the API server is briefly unreachable) is
  transient: the Pod stays Pending and retries rather than failing terminally.
- A projected volume needs the node ephemeral root configured
  (`node.volumes.root`), the same backing storage as a configMap volume.

### Validation

- `pkg/provider/projected_test.go` covers projected service-account volume
  translation: the three materialized files and their content/mode, the
  read-only mount at the standard path, default vs explicit ServiceAccount and
  audience, expiration pass-through and clamping, the Pod-bound token request,
  the tolerated no-issuer path, and the retryable token-request error.
- Manual in-cluster smoke: create a Pod with RBAC bound to its ServiceAccount and
  confirm a simple client (e.g. `kubectl auth can-i` from inside, or a
  client-go in-cluster config) reaches the API server using the materialized
  files.

---

## Health probes (#50)

MacVz honors a container's `startupProbe`, `readinessProbe`, and
`livenessProbe`. Each configured probe runs in its own loop for the lifetime of
the workload's micro-VM, on the probe's own `periodSeconds` cadence after its
`initialDelaySeconds`, and acts on `failureThreshold`/`successThreshold` exactly
as the kubelet does.

### Supported handlers

| Handler | How MacVz evaluates it |
| ------- | ---------------------- |
| `exec`     | Runs the command inside the micro-VM through the runtime exec path; exit 0 is success. |
| `httpGet`  | HTTP GET to the Pod (or `host` override) on the resolved port and path; any 2xx/3xx is success. HTTPS skips certificate verification and redirects are not followed, matching the kubelet. |
| `tcpSocket`| TCP connect to the Pod (or `host` override) on the resolved port; a successful connect is success. |

Named ports are resolved against the container's `ports`. A `grpc` probe — or
any handler MacVz does not recognize — is ignored: the container behaves as if
that probe were absent (an unsupported **startup** probe does not pin the
container as forever-unstarted).

### What each probe gates

- **Startup** suspends readiness and liveness until it first succeeds. While it
  is pending the container reports `Started: false` and is never Ready, so it
  stays out of Service endpoints. A startup probe that fails past its
  `failureThreshold` kills the workload, exactly like a liveness failure.
- **Readiness** drives the container's `Ready` flag and therefore EndpointSlice
  membership. A Pod becomes a Service endpoint only once it is Running, has a
  Pod IP, and (when configured) its readiness probe passes; a later readiness
  failure removes it from endpoints without restarting it.
- **Liveness** restarts the workload when it fails past its `failureThreshold`.
  The restart goes through the same micro-VM rebuild and backoff as an ordinary
  exit (see above) and respects `restartPolicy`: under `Always`/`OnFailure` the
  VM is rebuilt and `RestartCount` increments; under `Never` the workload is
  killed and the Pod becomes `Failed`.

### Timing and limitations

- Probe timing fields are whole seconds, as in Kubernetes. `successThreshold` is
  pinned to 1 for startup and liveness probes (kubelet behavior); readiness may
  require multiple consecutive successes.
- Probes are bound to one micro-VM. When a workload is restarted, its probers are
  cancelled and fresh ones start against the new VM, so startup and readiness are
  re-evaluated from scratch.
- `httpGet`/`tcpSocket` probes are dialed from the node; they reach the workload
  over the same Pod-network path that makes the Pod addressable (#22).

### Validation

- `pkg/provider/probe_test.go` covers readiness gating (and dropping back out of
  readiness), the no-readiness-probe default, startup gating of readiness,
  liveness-driven restart under `Always`, the `Never`-policy liveness failure
  surfacing `Failed`, an `httpGet` readiness probe against a live server, the
  ignored unsupported (gRPC) startup probe, named-port resolution, and prober
  teardown on Pod deletion.
- Manual: roll out a Deployment whose Pod template carries readiness and liveness
  probes and confirm endpoints track readiness and a wedged container is
  restarted.

## securityContext (#52)

MacVz runs each Pod in a dedicated Linux micro-VM — its own kernel, hardware
isolation — which is a stronger boundary than a shared-kernel container. The
provider honors `securityContext` field by field: it maps the fields the runtime
can enforce, accepts the fields the VM boundary already satisfies, and **rejects
the rest with a terminal `Failed` status** so a request never silently no-ops.

### Mapped to the runtime (enforced)

| Field | Behavior |
| --- | --- |
| `runAsUser` (+ `runAsGroup`) | Passed to `apple/container` as `--user uid[:gid]`. Container-level values override pod-level. |
| `readOnlyRootFilesystem: true` | Mounts the guest root filesystem read-only (`--read-only`). |
| `capabilities.add` / `capabilities.drop` | Passed as `--cap-add` / `--cap-drop` (names normalized to the `CAP_*` form the runtime expects; `ALL` is passed through). |

### Accepted as a no-op (satisfied by VM isolation)

These are accepted so hardened ("restricted" Pod Security Standard) workloads run,
because the micro-VM boundary already protects the host at least as strongly:

- `privileged: false`, `allowPrivilegeEscalation`
- `runAsNonRoot` — treated as a declaration. It is **enforced only when paired
  with `runAsUser`**; without an explicit non-zero UID, MacVz does not inspect the
  image's default user, so set `runAsUser` when you need the guarantee.
- `seccompProfile` and `appArmorProfile` of type `RuntimeDefault` or `Unconfined`
- pod `fsGroup`, `supplementalGroups` — guest volumes are already private to the
  Pod's VM, so no host-side ownership remap is applied.

### Rejected (terminal, with a precise reason)

- `privileged: true` — MacVz grants no host-device privilege; the micro-VM is the
  isolation boundary.
- `seLinuxOptions`, `windowsOptions`
- `procMount` other than `Default`
- `seccompProfile` / `appArmorProfile` of type `Localhost` — a node-local profile
  cannot be loaded into the guest.
- pod `sysctls`
- contradictory identity requests: `runAsNonRoot: true` with `runAsUser: 0`, or
  `runAsGroup` without `runAsUser` (the runtime sets the group through the user
  spec).

### Validation

- `pkg/provider/securitycontext_test.go` covers the mapped fields (user/group,
  read-only root, capability add/drop with name normalization), pod-over-container
  precedence, the accepted hardening no-ops, every rejection reason, and the
  terminal `Failed` status for a privileged container.
- `pkg/runtime/container/driver_test.go` covers the `--user` / `--read-only` /
  `--cap-add` / `--cap-drop` flag emission and their absence by default.
- Manual: apply a Deployment whose template sets `runAsUser`/`runAsGroup`,
  `readOnlyRootFilesystem`, and `capabilities`, and confirm the guest process
  runs as that user with a read-only root and the expected capabilities; apply a
  `privileged: true` Pod and confirm it is rejected with a clear status.
