# CRI-L8-4: Image lifecycle, cache, GC, and architecture handling (#143)

Parent: #141 (CRI-L8 k3s compatibility hardening and broader validation).

This report hardens and documents image behavior for ordinary k3s workloads on
the LinuxPod-backed CRI path. It is scoped to the experimental CRI adapter
(`pkg/criserver`) and its apple/container image backend (`pkg/runtime/container`).
The shipped Virtual Kubelet / apple-container provider path is untouched.

## Scope recap

- Pull-by-tag, pull-by-digest, image reuse / cache-hit behavior, and missing-image
  error mapping.
- arm64 compatibility and clear rejection/diagnostics for an unsupported image
  architecture.
- kubelet image GC / list / remove interactions.
- Capturing the real apple/container error text in stable tests and docs.

## Two ImageService surfaces

The CRI adapter exposes two ImageService implementations, both validated here:

1. **apple/container ImageService** (`pkg/criserver/image.go`, backed by
   `pkg/runtime/container`). This is the content-addressable path: it drives
   `image pull/inspect/ls/delete`, resolves a runtime-usable image ref, verifies a
   bootable arm64 (or amd64-via-Rosetta) variant at pull, and reports honest
   ImageFsInfo from the data-root filesystem.

2. **LinuxPod minimal ImageService** (`pkg/criserver/linuxpod_service.go`). On the
   LinuxPod path the real pull and architecture verification happen **inside the
   helper** when it stages a rootfs, so the adapter's ImageService is an honest
   thin record that satisfies the kubelet `PullImage → CreateContainer →
   RemoveImage` ordering. It is honest about being a record, not a store.

## What the adapter guarantees (CRI contract)

| Behavior | apple/container path | LinuxPod path |
|---|---|---|
| Pull-by-tag | resolves to image ID | records the reference |
| Pull-by-digest | resolves to the digest ref | records the digest reference |
| Cache reuse (repeat pull) | stable ID, no duplicate list entry | dedup'd, single list entry |
| ImageStatus absent | empty (nil image), non-error | empty (nil image), non-error |
| RemoveImage | idempotent; absent → no-op success | idempotent; reflected in list |
| Missing image | mapped from runtime error (see note) | n/a (helper owns content) |
| Unsupported arch | `FailedPrecondition` + actionable text | helper-side, surfaced at create |
| ImageFsInfo | honest data-root usage | zeroed, portable `/` mountpoint |

## Architecture handling

macvz boots arm64 Linux micro-VMs on Apple Silicon, so an image must advertise a
`linux/arm64` variant (or `linux/amd64` when Rosetta-for-Linux is enabled). The
driver verifies the bootable variant at **Pull** (`selectPlatform`), not later at
create time, and returns `runtime.ErrIncompatibleArch` — which the CRI layer maps
to `codes.FailedPrecondition` with the diagnostic preserved. An unsupported-arch
image therefore never reaches `CreateContainer`.

## Missing-image error mapping (a documented registry quirk)

A typo'd or genuinely absent image surfaces as a runtime error that the CRI layer
maps by class: `runtime.ErrNotFound → codes.NotFound`, `ErrIncompatibleArch →
FailedPrecondition`, `ErrNotReady → Unavailable`, else `Internal`.

**Note (Docker Hub):** Docker Hub returns **HTTP 401 Unauthorized** — not 404 — for
a non-existent repository/tag, to avoid leaking whether a private repo exists. The
backend therefore reports a missing Docker Hub image as a generic runtime error
(mapped to `codes.Internal`) rather than `NotFound`. This is correct registry
behavior, not an adapter bug: mapping 401 → NotFound would mask genuine auth
failures. Registries that return a real 404 (with stderr containing "not found")
do map to `NotFound`. Either way the pull fails loudly with the real registry text
attached, so kubelet enters ImagePullBackOff instead of treating the typo as
success.

## Evidence

### Hermetic (default `go test`, no backend required)

apple/container ImageService — `pkg/criserver/image_lifecycle_test.go`:

- `TestPullImageMapsMissingImageToNotFound` — `ErrNotFound` → `codes.NotFound`.
- `TestPullImageMapsIncompatibleArchToFailedPrecondition` — `ErrIncompatibleArch`
  → `codes.FailedPrecondition`, diagnostic names `linux/arm64`.
- `TestPullImageByDigestResolvesDigestRef` — pull-by-digest returns the resolved
  digest ref.
- `TestPullImageCacheReuseIsIdempotent` — repeat pull keeps a stable ID and one
  ListImages entry.

LinuxPod minimal ImageService — `pkg/criserver/linuxpod_image_test.go`:

- `TestLinuxPodImagePullByTagAndDigest` — tag and digest refs both recorded and
  queryable with kubelet-required ID + non-zero size.
- `TestLinuxPodImagePullReuseIsDeduplicated` — repeated pull → single list entry.
- `TestLinuxPodImageStatusAbsentIsEmptyNotError` — unpulled image reports absent.
- `TestLinuxPodImageRemoveIsIdempotentAndListed` — remove drops the record, is
  idempotent on an absent image, and is reflected in ListImages.

Existing coverage this builds on: `pkg/criserver/image_test.go` (status/list/
remove/filter/ImageFsInfo/auth), `pkg/runtime/container/image_test.go` (inspect/
list/remove parsing, digest reformatting, not-found), and
`pkg/runtime/container/arch_test.go` (platform selection, Rosetta).

### Gated/live (real apple/container backend)

`pkg/criserver/image_integration_test.go::TestLiveImageCacheReuseArchAndDigest`
(`MACVZ_CRI_INTEGRATION=1`) proves on a real backend: cache reuse (cold vs warm
pull, stable ID, one list entry), pull-by-digest resolution, architecture
rejection with the real diagnostic, and missing-image behavior with the real
registry text.

**Run on a local Apple Silicon host (apple/container 1.0.0, macOS 26):**

```
$ MACVZ_CRI_INTEGRATION=1 go test ./pkg/criserver/ \
    -run TestLiveImageCacheReuseArchAndDigest -v -count=1 -timeout 12m
=== RUN   TestLiveImageCacheReuseArchAndDigest
  cache reuse: cold pull 45.43s, warm pull 2.06s; ID cold=warm=
    docker.io/library/alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
  pull-by-digest resolved: docker.io/library/alpine@sha256:d9e853…b6bc -> ID (same)
  architecture rejection diagnostic (real backend):
    PullImage: pull image: runtime: image architecture incompatible:
    image "docker.io/amd64/alpine:3.20" has no linux/arm64 variant
    (found: linux/amd64); enable Rosetta-for-Linux (runtimeRosetta: true)
    to run amd64 images on Apple Silicon, or use an arm64/multi-arch image
  missing-image diagnostic (real backend): code=Internal
    msg=… Error: HTTP request to https://registry-1.docker.io/v2/library/
    macvz-nonexistent-image/manifests/does-not-exist failed with response:
    401 Unauthorized … no credentials found for host registry-1.docker.io
--- PASS: TestLiveImageCacheReuseArchAndDigest (56.26s)
```

This confirms the cold→warm cache hit (45.4s → 2.1s, identical digest ID), the
arm64-naming arch diagnostic mapped to `FailedPrecondition`, and the documented
Docker Hub 401 missing-image behavior.

The existing `TestLiveImageServiceLifecycle` (pull → status → list → ImageFsInfo →
remove → absent) also passes against the same backend, covering the kubelet GC /
list / remove interactions end to end.

The `test@192.168.1.122` operator run uses the same gated command on the project
test Mac; the behaviors are backend-identical to the local Apple Silicon run above.

## Validation

- `go test ./pkg/criserver/... ./pkg/runtime/...` green (hermetic).
- `go vet ./pkg/...` green.
- Gated `TestLiveImageCacheReuseArchAndDigest` and `TestLiveImageServiceLifecycle`
  PASS against apple/container 1.0.0 on Apple Silicon.

## Non-goals honored

- No change to the shipped Virtual Kubelet / apple-container provider path.
- No new runtime assumptions leak outside `pkg/runtime` / the CRI boundaries.
- No host route / pf / scheduler / control-plane changes.
