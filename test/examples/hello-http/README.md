# hello-http — minimal public HTTP app on MacVz (#61)

The simplest possible proof that MacVz serves **real HTTP traffic you can see in
a browser**: a stock public `nginx` image (arm64 multi-arch, no custom build, no
registry auth) running inside a MacVz micro-VM, fronted by a ClusterIP Service,
reached over `kubectl port-forward`.

This is the P8 "minimal public HTTP application" demo. For the larger,
multi-Deployment compatibility workload (ConfigMaps, Secrets, probes,
ServiceAccounts, in-cluster Service consumption), see
[`test/e2e/p6-compat`](../../e2e/p6-compat/).

## What it deploys

All objects live in the `macvz-hello` namespace (see [`manifests/`](manifests/)):

| Object | Kind | Purpose |
| --- | --- | --- |
| `hello-page` | ConfigMap | The HTML landing page, as an `envsubst` template |
| `hello` | Deployment | Stock `nginx:1.27-alpine`; renders the page with the Pod/Node name via the Downward API, then serves it |
| `hello` | Service (ClusterIP) | Stable target for `kubectl port-forward svc/hello` |

The page prints which **pod** and **virtual node** answered the request, so a
browser refresh makes single-node vs. multi-node load-balancing visible.

## Quick start

```bash
export KUBECONFIG=/path/to/kubeconfig   # the cluster your MacVz node joined
cd test/examples/hello-http
./run.sh                                # apply, verify HTTP over port-forward, tear down
```

A passing run prints `PASS hello-http demo: all checks passed`. `run.sh` exercises
the same access path a human uses — `kubectl port-forward` + an HTTP GET — so it
gates CI without a real browser.

## See it in a browser

Leave the demo running, then open the browser path manually:

```bash
MACVZ_HELLO_KEEP=1 ./run.sh
kubectl -n macvz-hello port-forward svc/hello 8080:80
```

Then open **<http://127.0.0.1:8080/>**. You should see the MacVz hello card
naming the pod and virtual node that served it:

```
It works on MacVz ✓
A public nginx image, served from an Apple Silicon micro-VM.
  Served by pod    hello-xxxxxxxxxx-yyyyy
  On virtual node  macvz-a
```

Stop the forward with `Ctrl-C` (or `kill <pid>`), then clean up with
`kubectl delete namespace macvz-hello`.

## Single-node and multi-node

- **Single node** (default): `replicas: 1`. One micro-VM serves every request.
- **Multi-node baseline**: scale up so Pods spread across virtual nodes —

  ```bash
  MACVZ_HELLO_REPLICAS=3 MACVZ_HELLO_KEEP=1 ./run.sh
  ```

  The Service load-balances across the replicas; refreshing the browser cycles
  the "Served by pod / On virtual node" lines as different micro-VMs answer.
  `kubectl port-forward` pins one backing Pod per session, so reconnect the
  forward (or use `kubectl -n macvz-hello get pods -o wide`) to observe the
  spread across nodes.

## How browser access works

MacVz has no kube-proxy and no cloud LoadBalancer; the browser-visible path is
`kubectl port-forward`. The kubelet runs on the same Mac as the Pod's micro-VM,
so it dials the guest's address directly and proxies the stream
([`pkg/provider/portforward.go`](../../../pkg/provider/portforward.go)).
`kubectl port-forward svc/hello` resolves the Service to a backing Pod and
forwards into that micro-VM.

## Manual verification cheatsheet

```bash
kubectl -n macvz-hello get pods -o wide          # Pods Running, on virtual-kubelet nodes
kubectl -n macvz-hello logs -l app=hello         # "macvz-hello nginx starting pod=... node=..."
kubectl -n macvz-hello exec deploy/hello -- \
  wget -qO- http://127.0.0.1/                     # HTTP from inside the micro-VM
kubectl -n macvz-hello port-forward svc/hello 8080:80   # then GET http://127.0.0.1:8080/
```

## Environment knobs

`run.sh` honours: `KUBECONFIG`, `KUBECTL`, `MACVZ_HELLO_NAMESPACE`,
`MACVZ_HELLO_IMAGE`, `MACVZ_HELLO_REPLICAS`, `MACVZ_HELLO_PORT`,
`MACVZ_HELLO_TIMEOUT`, `MACVZ_HELLO_DIAG_DIR`, `MACVZ_HELLO_KEEP` (see the header
of [`run.sh`](run.sh) for details).
