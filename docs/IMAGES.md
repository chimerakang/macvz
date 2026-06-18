# Image Architecture & Rosetta-for-Linux

MacVz boots Linux micro-VMs on Apple Silicon, so workload images must provide a
guest the host can run. This is the operator-facing guide for issue #27
(**P4 — Hardening & Beta**).

## Supported architectures

| Image variant | Behavior | Requires |
| --- | --- | --- |
| `linux/arm64` | runs natively; always preferred | — |
| `linux/amd64` | runs under Rosetta translation | `runtimeRosetta: true` + host Rosetta |
| anything else (`arm/v7`, `windows/*`, …) | rejected | — |

For a multi-arch image, MacVz always selects the native `linux/arm64` variant
when present — even with Rosetta enabled — so you never pay translation cost for
an image that ships arm64.

## How selection works

1. **Pull** inspects the image and validates it has a bootable variant under the
   active policy. An image with no runnable variant is rejected up front with an
   actionable error rather than failing later with the runtime's cryptic
   platform message.
2. **Create** pins the platform explicitly when Rosetta is enabled:
   `--platform linux/arm64` for native images, or
   `--platform linux/amd64 --rosetta` for amd64 images. (The runtime defaults to
   the host architecture and will not auto-select amd64, so the platform must be
   spelled out.) With Rosetta disabled, no platform is pinned and an arm64-less
   image is rejected.

## Enabling Rosetta

Rosetta-for-Linux is **off by default**. To run amd64 images:

```yaml
# macvz-kubelet config
runtimeRosetta: true
```

Requirements:

- Rosetta must be installed on the host (`softwareupdate --install-rosetta`).
- Apple Silicon Mac (Rosetta-for-Linux is an Apple Virtualization feature).

Verify with the bundled smoke check:

```sh
MACVZ_INTEGRATION=1 go test ./pkg/runtime/container/ -run TestRosettaIntegration -v
# boots docker.io/amd64/alpine and asserts `uname -m` == x86_64
```

## Failure messages

Pods that can never run on this node get a terminal `Failed` status with reason
`UnsupportedPodSpec` / `ImageArchitectureMismatch` and an actionable message:

- amd64-only image, Rosetta **off**:
  > image "…/amd64/alpine" has no linux/arm64 variant (found: linux/amd64);
  > enable Rosetta-for-Linux (runtimeRosetta: true) to run amd64 images on Apple
  > Silicon, or use an arm64/multi-arch image
- no linux variant at all:
  > image "…" has no linux/arm64 [or linux/amd64] variant (found: …); macvz boots
  > arm64 micro-VMs on Apple Silicon

## Guidance for multi-arch images

- Prefer images published as multi-arch manifests including `linux/arm64` (most
  official Docker Hub images are). These run natively with no configuration.
- For amd64-only third-party images, enable `runtimeRosetta` and accept the
  translation overhead, or rebuild the image for arm64.
- Performance-sensitive workloads should ship a native arm64 variant; Rosetta is
  a compatibility bridge, not a performance path.

## Known limitations

- Rosetta is a node-wide policy, not per-Pod. A Pod cannot opt individual
  containers in or out of translation.
- Only `linux/arm64` and `linux/amd64` are recognized; other architectures are
  rejected.
- Architecture is resolved from the image manifest at Pull/Create time; images
  without a usable manifest are reported as not found.
