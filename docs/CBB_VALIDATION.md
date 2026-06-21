# Defining a Cloud-Barista (CBB) arm64-compatible subset on MacVz (#64)

This document is the P8 **scoping and compatibility analysis** for running
Cloud-Barista (CBB) on MacVz. It defines which CBB components map onto MacVz's
virtual-node model on `linux/arm64`, lists the components that are *not* yet
supportable and why, and records the required MacVz features so the gaps can
become roadmap items.

It is deliberately an analysis, not a running service. P8's goal is to start
validating a realistic multi-service platform **without pretending the
unsupported GPU/amd64/stateful pieces will work immediately** — so the
deliverable is a defined subset plus an honest blocker list, not a permanent
CBB deployment.

## Why this issue exists

P8 ("Real App Validation") proves MacVz runs useful Kubernetes applications, not
just toy Pods. The other P8 issues each exercise one shape of real workload:

| Issue | Workload shape | What it proves |
| --- | --- | --- |
| #61 hello-http | one stock public image + ClusterIP | basic app + browser-visible Service |
| #62 guestbook | a few Deployments/Services/ConfigMaps/Secrets | small multi-tier app |
| #63 Headlamp | single-container management UI + in-cluster API | control-plane app talking to the API server |
| **#64 Cloud-Barista** | a **multi-service platform** with backing stores | a realistic microservice system, with explicit out-of-scope pieces |

Cloud-Barista is the most architecturally demanding of the four: it is a set of
independent Go microservices (CB-Spider, CB-Tumblebug, CB-MapUI, …) plus
stateful backing stores (etcd/key-value store, RDB, time-series + streaming for
monitoring). That mix is exactly where MacVz's model — **one container per
micro-VM, no kube-proxy, VirtioFS volumes, no privileged/hostPath escape
hatches** — meets a real platform. #64 is the issue that draws the supported
line through that platform instead of claiming the whole thing runs.

## What "CBB" refers to

CBB = **Cloud-Barista** (<https://github.com/cloud-barista>), an open-source
multi-cloud platform built from `CB-`prefixed microservices. The relevant
components for an arm64/MacVz subset:

| Component | Role | Shape |
| --- | --- | --- |
| CB-Spider | Cloud driver abstraction (one API over many CSPs) | single Go server |
| CB-Tumblebug | Multi-cloud infra/service orchestration | single Go server + key-value store |
| CB-MapUI | Web map UI for Tumblebug | single web container |
| CB-Dragonfly | Multi-cloud monitoring | Go server **+ InfluxDB + Kafka/ZooKeeper** |
| Backing stores | etcd / MariaDB-MySQL / InfluxDB | stateful, persistent volumes |

> Image architecture (arm64 vs. amd64-only) and exact tags must be confirmed
> against the upstream registry at validation time; this analysis classifies by
> component *shape*, which is what determines MacVz fit. Where a published
> arm64 image is missing, the component drops to the "blocked — needs multi-arch
> image" bucket regardless of its shape.

## The defined arm64-compatible subset

The supported subset is the **stateless control microservices**, each of which
is a single container that needs only features MacVz already provides:

- **CB-Spider** — single-container Go API server.
- **CB-Tumblebug** — single-container Go server (pointed at an external or
  in-cluster key-value store; see backing-store note below).
- **CB-MapUI** — single-container web UI fronting Tumblebug.

How the subset maps onto MacVz:

| CBB subset requirement | MacVz feature | Status |
| --- | --- | --- |
| Run as controller-managed Pods | Deployment / restartPolicy Always (#45) | ✅ |
| One container per Pod | one-container-per-micro-VM | ✅ (each CB service is single-container) |
| arm64 image | arm64 image verification / Rosetta handling (#12, #27) | ⚠️ verify per image at validation time |
| Config via env / files | ConfigMap env + volume projection (#46, #48) | ✅ |
| Secrets (CSP credentials, tokens) | Secret env + read-only secret volumes (#47) | ✅ |
| Private registry (if used) | imagePullSecrets (#49) | ✅ |
| Service-to-service (Spider ↔ Tumblebug ↔ MapUI) | ClusterIP routing without kube-proxy (#37) | ✅ |
| Health gating | readiness/liveness/startup probes (#50) | ✅ |
| Browser access to MapUI | `kubectl port-forward` (#28) | ✅ (via port-forward; no NodePort/LoadBalancer) |
| Hardening | field-by-field securityContext (#52) | ✅ for images that run non-root |

This is the "deploys on MacVz" subset the issue calls for: three single-container
services wired by ClusterIP, fronted by a port-forward to CB-MapUI. It needs
nothing MacVz does not already have, **except** a place to keep CB-Tumblebug's
state (next section).

### Backing-store caveat (the one real seam)

CB-Tumblebug needs a key-value store (etcd or its embedded store), and the wider
platform uses an RDB. Two ways to keep the subset honest:

1. **External store (recommended for first validation):** point CB-Tumblebug at
   a key-value/RDB endpoint *outside* MacVz (e.g. on the management host). The
   MacVz subset then stays purely stateless and the validation is clean.
2. **In-cluster store:** running etcd/MariaDB on MacVz requires durable
   `PersistentVolume`/`StatefulSet` semantics. MacVz volumes today are VirtioFS
   mounts (#26) without dynamic PV provisioning or stable per-replica identity,
   so a stateful store on-node is **out of scope for #64** and tracked as a
   blocker below.

## Unsupported components and why

| Component / feature | Why it does not fit MacVz today | Required MacVz feature |
| --- | --- | --- |
| **CB-Dragonfly monitoring stack** | Multi-service (collector + InfluxDB + Kafka + ZooKeeper); stateful and multi-container-shaped, well beyond the single-service subset | StatefulSet + dynamic PV provisioning; validated multi-service streaming stack |
| **In-cluster etcd / MariaDB / InfluxDB** | Need durable PVs and stable network identity; MacVz volumes are ephemeral VirtioFS mounts (#26) with no dynamic provisioner or StatefulSet identity | Dynamic `PersistentVolume` provisioning + `StatefulSet` support |
| **amd64-only images** | MacVz runs `linux/arm64` micro-VMs; amd64-only images need emulation and are explicitly not promised | Confirmed multi-arch images (build or upstream) |
| **GPU / NVIDIA workloads** | No GPU passthrough into Apple-silicon micro-VMs | (not planned) — keep on regular Linux nodes |
| **hostPath / privileged / host-network agents** | Rejected by MacVz's isolation model (#52); CSP agents that assume host access do not fit | (not planned) — keep on regular Linux nodes |
| **NodePort / LoadBalancer exposure** | MacVz has no kube-proxy; external exposure is port-forward only today | Ingress/exposure path without port-forward (open follow-up) |

## Recommended validation approach (when a cluster is available)

This issue is scoped to the definition above; an actual run is future work
(`#65` publishes runnable manifests + expected outputs). When a MacVz cluster is
live, the smoke path for the subset is:

1. Provide CB-Tumblebug an **external** key-value/RDB endpoint (keeps the subset
   stateless).
2. Deploy CB-Spider, CB-Tumblebug, CB-MapUI as single-replica Deployments with
   ClusterIP Services; gate each with HTTP readiness/liveness probes (#50).
3. Confirm in-cluster service-to-service resolution (MapUI → Tumblebug →
   Spider) over ClusterIP routing (#37).
4. `kubectl port-forward svc/cb-mapui` and confirm the UI loads in a browser.
5. Confirm CSP credentials flow through Secrets (#47) without appearing in logs.

Pass/fail is the subset coming `Ready` and CB-MapUI rendering over a real
port-forward; the backing store and CB-Dragonfly are explicitly excluded.

## Surfaced follow-ups (roadmap)

- **Dynamic PV provisioning + StatefulSet support** — the single biggest unlock;
  without it, every stateful CBB store (and CB-Dragonfly) stays off MacVz.
- **External exposure without port-forward** — shared with #63; needed before
  CB-MapUI is usable as a standing service rather than a smoke target.
- **Confirm/produce multi-arch images** for any CB component lacking arm64
  (track as separate issues per the issue's "document blockers as separate
  issues").
- **Multi-service streaming stack validation** (Kafka/ZooKeeper/InfluxDB) before
  CB-Dragonfly is considerable.

## Validation status

Analysis only: CBB component shapes classified against MacVz's feature set;
supported subset and blockers defined for roadmap planning. No CBB service was
deployed (per scope). Live subset smoke and runnable manifests are deferred to a
running cluster and to #65.
