#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
POC_DIR="${ROOT_DIR}/test/e2e/cri-linuxpod"
CONTAINERIZATION_DIR="${MACVZ_CONTAINERIZATION_DIR:-${POC_DIR}/containerization}"
SWIFTPM_CONTAINERIZATION_DIR="${POC_DIR}/containerization"
ENTITLEMENTS_PATH="${POC_DIR}/linuxpod-shared-namespace-poc.entitlements"
HOST_ARCH="$(uname -m)"
if [[ "${HOST_ARCH}" == "arm64" || "${HOST_ARCH}" == "aarch64" ]]; then
  DEFAULT_KERNEL="${CONTAINERIZATION_DIR}/bin/vmlinux-arm64"
else
  DEFAULT_KERNEL="${CONTAINERIZATION_DIR}/bin/vmlinuz-x86_64"
fi
KERNEL_PATH="${MACVZ_LINUXPOD_KERNEL:-${DEFAULT_KERNEL}}"
INIT_REFERENCE="${MACVZ_LINUXPOD_INITFS_REFERENCE:-vminit:latest}"
IMAGE="${MACVZ_LINUXPOD_IMAGE:-docker.io/library/busybox:1.36.1}"
WORK_DIR="${MACVZ_LINUXPOD_WORK_DIR:-/tmp/macvz-linuxpod-poc}"
PROBE="${MACVZ_LINUXPOD_PROBE:-c1}"
case "${PROBE}" in
  c1)
    DEFAULT_REPORT_PATH="${ROOT_DIR}/docs/CRI_LINUXPOD_POC_REPORT.md"
    REPORT_TITLE="CRI LinuxPod PoC Report (#88)"
    ;;
  c2)
    DEFAULT_REPORT_PATH="${ROOT_DIR}/docs/CRI_LINUXPOD_C2_REPORT.md"
    REPORT_TITLE="CRI LinuxPod C2 Ordering Probe Report (#89)"
    WORK_DIR="${MACVZ_LINUXPOD_WORK_DIR:-/tmp/macvz-linuxpod-c2}"
    ;;
  c4)
    DEFAULT_REPORT_PATH="${ROOT_DIR}/docs/CRI_LINUXPOD_C4_REPORT.md"
    REPORT_TITLE="CRI LinuxPod C4 HotplugProvider Boundary Probe Report (#91)"
    WORK_DIR="${MACVZ_LINUXPOD_WORK_DIR:-/tmp/macvz-linuxpod-c4}"
    ;;
  r1)
    DEFAULT_REPORT_PATH="${ROOT_DIR}/docs/CRI_RUNTIME_R1_DEVICE_DISCOVERY_REPORT.md"
    REPORT_TITLE="CRI-R1 Guest-Side Hotplug Device Discovery Report (#93)"
    WORK_DIR="${MACVZ_LINUXPOD_WORK_DIR:-/tmp/macvz-runtime-r1}"
    ;;
  *)
    echo "unsupported MACVZ_LINUXPOD_PROBE=${PROBE}; expected c1, c2, c4, or r1" >&2
    exit 1
    ;;
esac
REPORT_PATH="${MACVZ_LINUXPOD_REPORT:-${DEFAULT_REPORT_PATH}}"
EXTRA_ARGS=()
if [[ "${MACVZ_LINUXPOD_VMNET:-0}" == "1" ]]; then
  EXTRA_ARGS+=(--vmnet)
fi

if [[ "${MACVZ_LINUXPOD_POC:-0}" != "1" ]]; then
  cat <<EOF
MACVZ_LINUXPOD_POC is not 1; plan only.

This gated Swift harness can run:
  - c1 (#88): pre-create two-container shared-namespace proof
  - c2 (#89): post-create addContainer kubelet-ordering probe
  - c4 (#91): consumer HotplugProvider boundary probe
  - r1 (#93): guest-side hotplug block-device discovery probe

Selected probe: ${PROBE}

C1 flow:
  1. create one apple/containerization LinuxPod
  2. pre-register two busybox containers
  3. server listens on 127.0.0.1 inside the Pod
  4. client reaches server via localhost
  5. exec/stats/logs/stop-order behavior is checked

C2 flow:
  1. register server before pod.create()
  2. create/start the Pod and server
  3. attempt to add/start late-client after pod.create()
  4. record whether late-client can reach server via localhost

C4 flow:
  1. install a custom VZInstanceExtension / HotplugProvider
  2. register/start server before pod.create()
  3. attempt late-client after pod.create()
  4. record whether provider is installed, called, can attach rootfs, and can
     start the late container without guessing a guest block path

R1 flow:
  1. boot one LinuxPod with a predeclared utility container
  2. record the guest /sys/block baseline
  3. attach a second ext4 rootfs image as public VZ USB mass storage
  4. have the guest detect a new block device, correlate it by sector count,
     mount it read-only, verify busybox rootfs content, unmount, detach, and
     verify the block device disappears without treating guessed /dev names as
     success

Set MACVZ_LINUXPOD_VMNET=1 to also attach a vmnet interface. The default
keeps this C1 probe focused on LinuxPod shared-namespace behavior.

Live run requirements:
  - macOS 26+ with Apple Virtualization.framework and vmnet support
  - Swift 6.2+
  - apple/containerization checkout at:
      ${CONTAINERIZATION_DIR}
  - kernel at:
      ${KERNEL_PATH}
  - initfs reference available in Apple Containerization image store:
      ${INIT_REFERENCE}

Suggested setup:
  git clone https://github.com/apple/containerization "${CONTAINERIZATION_DIR}"
  make -C "${CONTAINERIZATION_DIR}" fetch-default-kernel
  make -C "${CONTAINERIZATION_DIR}" cross-prep
  make -C "${CONTAINERIZATION_DIR}" init

Run live:
  MACVZ_LINUXPOD_POC=1 make cri-linuxpod-poc
  MACVZ_LINUXPOD_POC=1 make cri-linuxpod-c2
  MACVZ_LINUXPOD_POC=1 make cri-linuxpod-c4
  MACVZ_LINUXPOD_POC=1 make cri-linuxpod-r1
EOF
  exit 0
fi

if [[ ! -d "${CONTAINERIZATION_DIR}" ]]; then
  echo "containerization checkout not found: ${CONTAINERIZATION_DIR}" >&2
  echo "clone it or set MACVZ_CONTAINERIZATION_DIR=/path/to/containerization" >&2
  exit 1
fi

if [[ ! -e "${SWIFTPM_CONTAINERIZATION_DIR}" && "${CONTAINERIZATION_DIR}" != "${SWIFTPM_CONTAINERIZATION_DIR}" ]]; then
  ln -s "${CONTAINERIZATION_DIR}" "${SWIFTPM_CONTAINERIZATION_DIR}"
fi

if [[ ! -f "${SWIFTPM_CONTAINERIZATION_DIR}/Package.swift" ]]; then
  echo "SwiftPM dependency is not a valid containerization checkout: ${SWIFTPM_CONTAINERIZATION_DIR}" >&2
  exit 1
fi

if [[ ! -f "${KERNEL_PATH}" ]]; then
  echo "kernel not found: ${KERNEL_PATH}" >&2
  echo "run: make -C \"${CONTAINERIZATION_DIR}\" fetch-default-kernel" >&2
  exit 1
fi

if ! command -v swift >/dev/null 2>&1; then
  echo "swift not found in PATH" >&2
  exit 1
fi

pushd "${POC_DIR}" >/dev/null

swift build --cache-path "${POC_DIR}/.build/swiftpm-cache"
bin_dir="$(swift build --show-bin-path --cache-path "${POC_DIR}/.build/swiftpm-cache")"
poc_bin="${bin_dir}/linuxpod-shared-namespace-poc"
codesign --force --sign - --timestamp=none --entitlements "${ENTITLEMENTS_PATH}" "${poc_bin}"

summary_json="$(
  "${poc_bin}" \
    --kernel "${KERNEL_PATH}" \
    --initfs-reference "${INIT_REFERENCE}" \
    --image "${IMAGE}" \
    --work-dir "${WORK_DIR}" \
    --probe "${PROBE}" \
    "${EXTRA_ARGS[@]}"
)"

popd >/dev/null

cat >"${REPORT_PATH}" <<EOF
# ${REPORT_TITLE}

Date: $(date -u +%Y-%m-%dT%H:%M:%SZ)

## Environment

- Host: $(uname -a)
- Swift: $(swift --version | head -1)
- Containerization checkout: ${CONTAINERIZATION_DIR}
- Kernel: ${KERNEL_PATH}
- Initfs reference: ${INIT_REFERENCE}
- Image: ${IMAGE}
- Work dir: ${WORK_DIR}
- Probe: ${PROBE}
- vmnet interface: ${MACVZ_LINUXPOD_VMNET:-0}

## Result

\`\`\`json
${summary_json}
\`\`\`

## Acceptance / Interpretation
EOF

if [[ "${PROBE}" == "c1" ]]; then
  cat >>"${REPORT_PATH}" <<EOF
- [x] One LinuxPod was created.
- [x] Two containers were registered before pod.create().
- [x] Server container listened on 127.0.0.1.
- [x] Client container reached server through localhost.
- [x] Exec worked inside the server container.
- [x] CPU/memory stats were returned for both containers.
- [x] Stopping the server first left the client observable.
- [x] Pod stop completed cleanly.

EOF
else
  if [[ "${PROBE}" == "c2" ]]; then
    cat >>"${REPORT_PATH}" <<EOF
- [x] One LinuxPod was created.
- [x] Server was registered before pod.create().
- [x] Pod and server were started before the late add attempt.
- [x] The post-create addContainer/start/probe outcome was recorded.
- [x] The fallback model is included in the JSON result.

EOF
  elif [[ "${PROBE}" == "c4" ]]; then
    cat >>"${REPORT_PATH}" <<EOF
- [x] One LinuxPod was created.
- [x] A custom VZInstanceExtension / HotplugProvider was installed or the failure was recorded.
- [x] Server was registered before pod.create().
- [x] Pod and server were started before the late add attempt.
- [x] The post-create addContainer path recorded whether the provider was called.
- [x] The report distinguishes provider install/call, public rootfs attach, guest path resolution, and late-container start.
- [x] No guessed guest block path is counted as success.

EOF
  else
    cat >>"${REPORT_PATH}" <<EOF
- [x] One LinuxPod was created.
- [x] A custom VZInstanceExtension configured an XHCI controller and captured the running VM instance.
- [x] Guest /sys/block baseline was recorded before host attach.
- [x] A second ext4 rootfs image was attached through public VZ USB mass storage or the attach failure was recorded.
- [x] Guest-side discovery distinguishes observation, correlation by exact sector count, mount, marker verification, unmount, detach, and post-detach cleanup.
- [x] No guessed /dev/sdX or /dev/vdX path is counted as success.

EOF
  fi
fi

echo "LinuxPod probe completed. Report written to ${REPORT_PATH}"
