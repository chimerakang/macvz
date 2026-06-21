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
REPORT_PATH="${MACVZ_LINUXPOD_REPORT:-${ROOT_DIR}/docs/CRI_LINUXPOD_POC_REPORT.md}"
EXTRA_ARGS=()
if [[ "${MACVZ_LINUXPOD_VMNET:-0}" == "1" ]]; then
  EXTRA_ARGS+=(--vmnet)
fi

if [[ "${MACVZ_LINUXPOD_POC:-0}" != "1" ]]; then
  cat <<EOF
MACVZ_LINUXPOD_POC is not 1; plan only.

This #88 PoC builds and runs a Swift LinuxPod shared-namespace probe:
  1. create one apple/containerization LinuxPod
  2. pre-register two busybox containers
  3. server listens on 127.0.0.1 inside the Pod
  4. client reaches server via localhost
  5. exec/stats/logs/stop-order behavior is checked

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
    "${EXTRA_ARGS[@]}"
)"

popd >/dev/null

cat >"${REPORT_PATH}" <<EOF
# CRI LinuxPod PoC Report (#88)

Date: $(date -u +%Y-%m-%dT%H:%M:%SZ)

## Environment

- Host: $(uname -a)
- Swift: $(swift --version | head -1)
- Containerization checkout: ${CONTAINERIZATION_DIR}
- Kernel: ${KERNEL_PATH}
- Initfs reference: ${INIT_REFERENCE}
- Image: ${IMAGE}
- Work dir: ${WORK_DIR}
- vmnet interface: ${MACVZ_LINUXPOD_VMNET:-0}

## Result

\`\`\`json
${summary_json}
\`\`\`

## Acceptance

- [x] One LinuxPod was created.
- [x] Two containers were registered before pod.create().
- [x] Server container listened on 127.0.0.1.
- [x] Client container reached server through localhost.
- [x] Exec worked inside the server container.
- [x] CPU/memory stats were returned for both containers.
- [x] Stopping the server first left the client observable.
- [x] Pod stop completed cleanly.

EOF

echo "LinuxPod PoC passed. Report written to ${REPORT_PATH}"
