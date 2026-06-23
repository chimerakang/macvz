#!/usr/bin/env bash
# run.sh builds the CRI-R17 LinuxPod helper stub and runs the Go<->Swift contract
# test against it (issue #124). The stub is a Foundation/Darwin-only Swift program
# that speaks the pkg/runtime/linuxpod NDJSON protocol over a unix socket with an
# in-memory lifecycle model mirroring the Go FakeBackend. It boots no real VM; it
# proves the backend contract is implementable in Swift and that the Go adapter
# can drive it across a real socket.
#
# Usage:
#   ./test/e2e/cri-linuxpod-helper/run.sh            # build stub + run contract test
#   ./test/e2e/cri-linuxpod-helper/run.sh --serve /tmp/h.sock   # just serve, for manual probing
set -euo pipefail

HELPER_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${HELPER_DIR}/../../.." && pwd)"

echo "==> Building LinuxPod helper stub (swift build)"
( cd "${HELPER_DIR}" && swift build -c debug )
BIN="${HELPER_DIR}/.build/debug/LinuxPodHelperStub"

if [[ "${1:-}" == "--serve" ]]; then
  SOCKET="${2:-/tmp/macvz-linuxpod-helper.sock}"
  echo "==> Serving on ${SOCKET} (Ctrl-C to stop)"
  exec "${BIN}" --socket "${SOCKET}"
fi

echo "==> Running Go<->Swift contract test"
cd "${ROOT_DIR}"
MACVZ_LINUXPOD_HELPER=1 MACVZ_LINUXPOD_HELPER_BIN="${BIN}" \
  go test ./pkg/runtime/linuxpod/ -run TestSwiftHelperStubContract -v
