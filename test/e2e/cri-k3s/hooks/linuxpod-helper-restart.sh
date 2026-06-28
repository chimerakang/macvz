#!/usr/bin/env bash
set -euo pipefail

# Safe MACVZ_RESTART_HELPER_CMD hook for linuxpod-inloop.sh/linuxpod-soak.sh.
#
# This restarts only the public linuxpod-helper router. It intentionally does
# not kill per-Pod `supervise-pod` children, because those own live Pod VMs after
# #139. Readiness is gated by a real NDJSON Ping response over the unix socket;
# merely observing a socket file is not enough because stale sockets can survive
# a router crash/restart window.

target="${MACVZ_HELPER_SSH_TARGET:-${1:-}}"
live_root="${MACVZ_LINUXPOD_LIVE_ROOT:-}"
service_dir="${MACVZ_LINUXPOD_SERVICE_DIR:-}"
socket="${MACVZ_LINUXPOD_HELPER_SOCKET:-}"
pid_file="${MACVZ_LINUXPOD_HELPER_PID_FILE:-}"
helper_bin="${MACVZ_LINUXPOD_HELPER_BIN:-}"
work_dir="${MACVZ_LINUXPOD_HELPER_WORK_DIR:-}"
kernel="${MACVZ_LINUXPOD_KERNEL:-}"
containerization_root="${MACVZ_LINUXPOD_CONTAINERIZATION_ROOT:-}"
initfs_reference="${MACVZ_LINUXPOD_INITFS_REFERENCE:-}"
image="${MACVZ_LINUXPOD_IMAGE:-}"
vmnet="${MACVZ_LINUXPOD_HELPER_VMNET:-1}"
timeout="${MACVZ_LINUXPOD_HELPER_READY_TIMEOUT:-60}"
default_token="__MACVZ_DEFAULT__"

remote_script='
set -eu
live_root="$1"
service_dir="$2"
socket="$3"
pid_file="$4"
helper_bin="$5"
work_dir="$6"
kernel="$7"
containerization_root="$8"
initfs_reference="$9"
image="${10}"
vmnet="${11}"
timeout="${12}"

[ "$live_root" != "__MACVZ_DEFAULT__" ] || live_root=""
[ "$service_dir" != "__MACVZ_DEFAULT__" ] || service_dir=""
[ "$socket" != "__MACVZ_DEFAULT__" ] || socket=""
[ "$pid_file" != "__MACVZ_DEFAULT__" ] || pid_file=""
[ "$helper_bin" != "__MACVZ_DEFAULT__" ] || helper_bin=""
[ "$work_dir" != "__MACVZ_DEFAULT__" ] || work_dir=""
[ "$kernel" != "__MACVZ_DEFAULT__" ] || kernel=""
[ "$containerization_root" != "__MACVZ_DEFAULT__" ] || containerization_root=""
[ "$initfs_reference" != "__MACVZ_DEFAULT__" ] || initfs_reference=""
[ "$image" != "__MACVZ_DEFAULT__" ] || image=""

[ -n "$live_root" ] || live_root="$HOME/macvz-cri-live-b99e050"
[ -n "$service_dir" ] || service_dir="$live_root/service-linuxpod"
[ -n "$socket" ] || socket="$service_dir/linuxpod-helper.sock"
[ -n "$pid_file" ] || pid_file="$service_dir/linuxpod-helper.pid"
[ -n "$helper_bin" ] || helper_bin="$live_root/test/e2e/cri-linuxpod/.build/arm64-apple-macosx/debug/linuxpod-helper.v5-signed"
[ -n "$work_dir" ] || work_dir="$service_dir/helper-work"
[ -n "$kernel" ] || kernel="$live_root/test/e2e/cri-linuxpod/containerization/bin/vmlinux-arm64"
[ -n "$containerization_root" ] || containerization_root="$live_root/test/e2e/cri-linuxpod/containerization/bin"
[ -n "$initfs_reference" ] || initfs_reference="vminit:latest"
[ -n "$image" ] || image="docker.io/library/busybox:1.36.1"

log_file="$service_dir/linuxpod-helper.log"

mkdir -p "$service_dir" "$work_dir"

if [ -s "$pid_file" ]; then
	old_pid="$(cat "$pid_file" 2>/dev/null || true)"
	case "$old_pid" in
		""|*[!0-9]*) ;;
		*) kill -TERM "$old_pid" 2>/dev/null || true ;;
	esac
fi

ps -axo pid=,command= |
	grep "[l]inuxpod-helper" |
	grep -- "--socket $socket" |
	grep -v " supervise-pod " |
	awk "{print \$1}" |
	while read -r pid; do
		[ -n "$pid" ] && kill -TERM "$pid" 2>/dev/null || true
	done

sleep 2

if [ -s "$pid_file" ]; then
	old_pid="$(cat "$pid_file" 2>/dev/null || true)"
	case "$old_pid" in
		""|*[!0-9]*) ;;
		*) kill -KILL "$old_pid" 2>/dev/null || true ;;
	esac
fi

rm -f "$socket"
cd "$live_root"
if [ "$vmnet" = 1 ] || [ "$vmnet" = true ]; then
	nohup "$helper_bin" \
		--socket "$socket" \
		--work-dir "$work_dir" \
		--kernel "$kernel" \
		--containerization-root "$containerization_root" \
		--initfs-reference "$initfs_reference" \
		--image "$image" \
		--vmnet >"$log_file" 2>&1 &
else
	nohup "$helper_bin" \
		--socket "$socket" \
		--work-dir "$work_dir" \
		--kernel "$kernel" \
		--containerization-root "$containerization_root" \
		--initfs-reference "$initfs_reference" \
		--image "$image" >"$log_file" 2>&1 &
fi
new_pid=$!
printf "%s\n" "$new_pid" >"$pid_file"

i=0
while [ "$i" -lt "$timeout" ]; do
	i=$((i + 1))
	if [ -S "$socket" ]; then
		resp="$(printf "%s\n" "{\"op\":\"Ping\"}" | nc -U "$socket" 2>/dev/null | head -n 1 || true)"
		case "$resp" in
			*"\"ok\":true"*)
				case "$resp" in
					*"\"simulated\":false"*)
						printf "linuxpod helper restarted pid=%s\n" "$new_pid"
						printf "%s\n" "$resp"
						exit 0
						;;
				esac
				;;
		esac
	fi
	if ! kill -0 "$new_pid" 2>/dev/null; then
		printf "linuxpod-helper exited before readiness; see %s\n" "$log_file" >&2
		tail -n 80 "$log_file" >&2 2>/dev/null || true
		exit 1
	fi
	sleep 1
done

printf "linuxpod-helper did not become ready within %ss; see %s\n" "$timeout" "$log_file" >&2
tail -n 80 "$log_file" >&2 2>/dev/null || true
exit 1
'

arg_live_root="${live_root:-$default_token}"
arg_service_dir="${service_dir:-$default_token}"
arg_socket="${socket:-$default_token}"
arg_pid_file="${pid_file:-$default_token}"
arg_helper_bin="${helper_bin:-$default_token}"
arg_work_dir="${work_dir:-$default_token}"
arg_kernel="${kernel:-$default_token}"
arg_containerization_root="${containerization_root:-$default_token}"
arg_initfs_reference="${initfs_reference:-$default_token}"
arg_image="${image:-$default_token}"

if [ -n "$target" ]; then
	printf '%s\n' "$remote_script" | ssh "$target" /bin/sh -s -- \
		"$arg_live_root" "$arg_service_dir" "$arg_socket" "$arg_pid_file" "$arg_helper_bin" "$arg_work_dir" \
		"$arg_kernel" "$arg_containerization_root" "$arg_initfs_reference" "$arg_image" "$vmnet" "$timeout"
else
	printf '%s\n' "$remote_script" | /bin/sh -s -- \
		"$arg_live_root" "$arg_service_dir" "$arg_socket" "$arg_pid_file" "$arg_helper_bin" "$arg_work_dir" \
		"$arg_kernel" "$arg_containerization_root" "$arg_initfs_reference" "$arg_image" "$vmnet" "$timeout"
fi
