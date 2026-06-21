#!/usr/bin/env bash
set -euo pipefail

CONTAINER_BIN="${CONTAINER_BIN:-container}"
IMAGE="${MACVZ_CRI_IMAGE:-docker.io/library/alpine:3.20}"
LIVE="${MACVZ_CRI_LIVE:-0}"
NAME="macvz-cri-feasibility-$$"

failures=0

section() {
  printf '\n== %s ==\n' "$1"
}

pass() {
  printf 'PASS %s\n' "$1"
}

fail() {
  printf 'FAIL %s\n' "$1"
  failures=$((failures + 1))
}

require_cmd() {
  if command -v "$1" >/dev/null 2>&1; then
    pass "found $1"
  else
    fail "missing $1"
  fi
}

expect_help() {
  local label="$1"
  shift
  if "$CONTAINER_BIN" "$@" --help >/dev/null 2>&1; then
    pass "$label help"
  else
    fail "$label help"
  fi
}

expect_status() {
  if "$CONTAINER_BIN" system status >/dev/null 2>&1; then
    pass "container system status"
  else
    fail "container system status"
  fi
}

cleanup_live() {
  "$CONTAINER_BIN" delete --force "$NAME" >/dev/null 2>&1 || true
}

run_live_probe() {
  section "Live lifecycle probe"
  trap cleanup_live EXIT

  "$CONTAINER_BIN" image pull "$IMAGE" >/dev/null
  pass "image pull $IMAGE"

  "$CONTAINER_BIN" create --name "$NAME" "$IMAGE" sleep 60 >/dev/null
  pass "create $NAME"

  "$CONTAINER_BIN" inspect "$NAME" >/dev/null
  pass "inspect created container"

  "$CONTAINER_BIN" start "$NAME" >/dev/null
  pass "start $NAME"

  "$CONTAINER_BIN" exec "$NAME" uname -m >/dev/null
  pass "exec in running container"

  "$CONTAINER_BIN" stats --format json --no-stream "$NAME" >/dev/null
  pass "stats sample"

  "$CONTAINER_BIN" logs -n 20 "$NAME" >/dev/null
  pass "logs"

  "$CONTAINER_BIN" stop --time 1 "$NAME" >/dev/null
  pass "stop $NAME"

  "$CONTAINER_BIN" delete --force "$NAME" >/dev/null
  trap - EXIT
  pass "delete $NAME"
}

section "Tooling"
require_cmd "$CONTAINER_BIN"
"$CONTAINER_BIN" --version || true

section "Runtime service"
expect_status

section "Required command surfaces"
expect_help "create" create
expect_help "start" start
expect_help "stop" stop
expect_help "delete" delete
expect_help "inspect" inspect
expect_help "list" list
expect_help "logs" logs
expect_help "exec" exec
expect_help "stats" stats
expect_help "image" image
expect_help "image pull" image pull
expect_help "image inspect" image inspect
expect_help "network" network

if [ "$LIVE" = "1" ]; then
  run_live_probe
else
  section "Live lifecycle probe"
  printf 'SKIP set MACVZ_CRI_LIVE=1 to pull %s and boot a micro-VM\n' "$IMAGE"
fi

section "Phase 0 result"
if [ "$failures" -eq 0 ]; then
  printf 'PASS CRI-P0 non-invasive probe completed\n'
else
  printf 'FAIL CRI-P0 probe found %d issue(s)\n' "$failures"
  exit 1
fi
