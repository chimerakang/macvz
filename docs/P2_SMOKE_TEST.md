# P2 MVP Smoke Test

This guide runs `macvz-kubelet` as a Virtual Kubelet node, schedules an Alpine
Pod onto it as an `apple/container` micro-VM, and verifies `kubectl logs` and
`kubectl exec`. It is the operator-facing acceptance path for **P2 — Virtual
Kubelet Provider MVP**.

## Prerequisites

- An Apple Silicon Mac with [`apple/container`](https://github.com/apple/container)
  installed and started: `container system start` (verify with
  `container system status`).
- A reachable Kubernetes cluster and a kubeconfig with permissions to register
  nodes and manage Pods (an admin kubeconfig is simplest for local dev; see
  [deployments/rbac.yaml](../deployments/rbac.yaml) for the scoped permissions).
- The cluster's API server must be able to reach this Mac's IP on the kubelet
  port (default `10250`) for `kubectl logs`/`exec` to work.

## 1. Apply RBAC (optional for admin kubeconfig)

```sh
kubectl apply -f deployments/rbac.yaml
```

## 2. Build macvz-kubelet

```sh
make build              # produces ./bin/macvz-kubelet
```

## 3. Configure

Copy and edit the example config:

```sh
cp config.example.yaml ./config.yaml
# Set nodeName, kubeconfigPath, and (for logs/exec) the serving TLS cert/key.
```

For `kubectl logs`/`exec`, generate a serving certificate whose SAN covers the
Mac's IP/hostname and is trusted by your cluster, then set `servingTLSCertFile`
and `servingTLSKeyFile` in `config.yaml`. A quick self-signed cert for local
testing:

```sh
mkdir -p pki
openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout pki/kubelet.key -out pki/kubelet.crt \
  -subj "/CN=$(hostname)" \
  -addext "subjectAltName=IP:$(ipconfig getifaddr en0),DNS:$(hostname)"
```

> Without a serving cert the node still registers and runs Pods, but
> `kubectl logs`/`exec` are unavailable (macvz-kubelet logs a warning at start).

## 4. Run the node

```sh
./bin/macvz-kubelet --config ./config.yaml
```

Expected startup logs:

```
"starting macvz-kubelet" ...
"apple/container runtime is ready"
"registering virtual node" node="<nodeName>" cpu="2" memory="4Gi" pods="20" ...
"virtual node registered and ready" node="<nodeName>"
"pod controller ready" node="<nodeName>"
"kubelet API server listening" addr=":10250"          # only with serving certs
```

In another terminal, confirm the node is present and Ready:

```sh
kubectl get nodes
# NAME           STATUS   ROLES   AGE   VERSION
# <nodeName>     Ready    agent   10s   <version>

kubectl describe node <nodeName>
# Shows Capacity/Allocatable (cpu/memory/pods), the
# virtual-kubelet.io/provider=macvz:NoSchedule taint, labels, and Ready=True.
```

## 5. Run Alpine and verify logs/exec

```sh
# Pin the node name in the manifest's nodeSelector if you scheduled by hostname.
kubectl apply -f deployments/alpine-smoke.yaml

kubectl get pod alpine-smoke -w           # wait for Running
```

`kubectl logs` (Alpine sleeps quietly, so exec is the clearer check):

```sh
kubectl exec alpine-smoke -- sh -c 'echo hello-from-macvz; uname -m'
# hello-from-macvz
# aarch64

kubectl logs alpine-smoke                 # streams the workload's output
```

A non-zero command surfaces its exit code coherently:

```sh
kubectl exec alpine-smoke -- sh -c 'exit 7'; echo "exit=$?"
# command terminated with exit code 7
# exit=7
```

Error cases return clear messages:

```sh
kubectl exec alpine-smoke -c nope -- true   # unknown container -> NotFound
kubectl logs missing-pod                     # unknown pod -> NotFound
```

## 6. Cleanup

```sh
# Remove the test Pod (tears down the micro-VM).
kubectl delete -f deployments/alpine-smoke.yaml

# Stop macvz-kubelet (Ctrl-C). The node lease stops renewing and Kubernetes
# marks the node NotReady, then removes it after the node-monitor grace period.
# To remove it immediately:
kubectl delete node <nodeName>

# Remove RBAC if applied.
kubectl delete -f deployments/rbac.yaml

# Optional: confirm no micro-VMs remain on the host.
container ls --all
```

## Troubleshooting

- **Node never becomes Ready**: check the kubeconfig and that the API server is
  reachable; a missing/invalid kubeconfig fails loudly at startup.
- **Pod stuck Pending**: confirm the Pod tolerates `virtual-kubelet.io/provider`
  and targets the node (nodeSelector/affinity).
- **Pod Failed with `UnsupportedPodSpec`**: the MVP runs single-container Pods
  only; multi-container, init containers, user volumes, and securityContext are
  rejected with a clear message (`kubectl describe pod`).
- **Pod Failed with `ImageArchitectureMismatch`**: the image has no linux/arm64
  variant; MacVz boots arm64 micro-VMs (amd64/Rosetta is deferred to P4).
- **`kubectl logs`/`exec` hang or error**: ensure serving certs are configured
  and the API server can reach the Mac on `kubeletPort`.
