#!/usr/bin/env bash
set -euo pipefail

# Safe MACVZ_RESTART_NETD_CMD hook for linuxpod-soak.sh.
#
# This reloads macvz-netd's config-derived policy through its existing unix
# socket. It does not use sudo, launchctl, route, pfctl, or route mutation.

target="${MACVZ_NETD_SSH_TARGET:-${1:-}}"
socket="${MACVZ_NETD_SOCKET:-/var/run/macvz-netd.sock}"

if [ -z "$target" ]; then
	printf 'usage: MACVZ_NETD_SSH_TARGET=user@host %s\n' "$0" >&2
	printf '   or: %s user@host\n' "$0" >&2
	exit 64
fi

remote_script='
set -eu
socket="$1"
if [ ! -S "$socket" ]; then
	printf "macvz-netd socket not found: %s\n" "$socket" >&2
	exit 66
fi
send() {
	printf "%s\n" "$1" | nc -U "$socket"
}
reload_resp="$(send "{\"protocol\":1,\"op\":\"reloadPolicy\"}")"
printf "%s\n" "$reload_resp"
case "$reload_resp" in
	*"\"err\""*|*"\"errorCode\""*) exit 1 ;;
	*"\"protocol\":1"*) ;;
	*) exit 1 ;;
esac
status_resp="$(send "{\"protocol\":1,\"op\":\"status\"}")"
printf "%s\n" "$status_resp"
case "$status_resp" in
	*"\"err\""*|*"\"errorCode\""*) exit 1 ;;
	*"\"policyReloadable\":true"*) ;;
	*) exit 1 ;;
esac
'

printf '%s\n' "$remote_script" | ssh "$target" /bin/sh -s -- "$socket"
