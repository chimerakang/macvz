#!/usr/bin/env bash
set -euo pipefail

# Safe MACVZ_RESTART_CRI_CMD hook for LinuxPod CRI in-loop/soak tests.
#
# This restarts only the macvz-cri process serving the LinuxPod test socket. It
# preserves the rootless remote log topology by always carrying
# --linuxpod-log-root, so kubelet logs keep resolving after CRI churn.

target="${MACVZ_CRI_SSH_TARGET:-${1:-}}"
live_root="${MACVZ_LINUXPOD_LIVE_ROOT:-}"
service_dir="${MACVZ_LINUXPOD_SERVICE_DIR:-}"
socket="${MACVZ_CRI_SOCKET:-}"
state_dir="${MACVZ_CRI_STATE_DIR:-}"
helper_socket="${MACVZ_LINUXPOD_HELPER_SOCKET:-}"
log_root="${MACVZ_LINUXPOD_LOG_ROOT:-}"
kubelet_pods_dir="${MACVZ_KUBELET_PODS_DIR:-}"
volume_allowed="${MACVZ_VOLUME_HOST_PATH_ALLOWED:-}"
pod_cidr="${MACVZ_POD_CIDR:-10.244.102.0/24}"
pod_network_interface="${MACVZ_POD_NETWORK_INTERFACE:-bridge100}"
pod_network_helper_socket="${MACVZ_POD_NETWORK_HELPER_SOCKET:-/var/run/macvz-netd.sock}"
pod_network_ingress_interface="${MACVZ_POD_NETWORK_INGRESS_INTERFACE:-en0}"
streaming_addr="${MACVZ_STREAMING_ADDR:-192.168.1.122:0}"
codesign_binary="${MACVZ_CRI_CODESIGN:-1}"
timeout="${MACVZ_CRI_READY_TIMEOUT:-100}"
default_token="__MACVZ_DEFAULT__"

remote_script='
set -eu
live_root="$1"
service_dir="$2"
socket="$3"
state_dir="$4"
helper_socket="$5"
log_root="$6"
kubelet_pods_dir="$7"
volume_allowed="$8"
pod_cidr="$9"
pod_network_interface="${10}"
pod_network_helper_socket="${11}"
pod_network_ingress_interface="${12}"
streaming_addr="${13}"
codesign_binary="${14}"
timeout="${15}"

[ "$live_root" != "__MACVZ_DEFAULT__" ] || live_root=""
[ "$service_dir" != "__MACVZ_DEFAULT__" ] || service_dir=""
[ "$socket" != "__MACVZ_DEFAULT__" ] || socket=""
[ "$state_dir" != "__MACVZ_DEFAULT__" ] || state_dir=""
[ "$helper_socket" != "__MACVZ_DEFAULT__" ] || helper_socket=""
[ "$log_root" != "__MACVZ_DEFAULT__" ] || log_root=""
[ "$kubelet_pods_dir" != "__MACVZ_DEFAULT__" ] || kubelet_pods_dir=""
[ "$volume_allowed" != "__MACVZ_DEFAULT__" ] || volume_allowed=""

[ -n "$live_root" ] || live_root="$HOME/macvz-cri-live-b99e050"
[ -n "$service_dir" ] || service_dir="$live_root/service-linuxpod"
[ -n "$socket" ] || socket="$service_dir/macvz-cri.sock"
[ -n "$state_dir" ] || state_dir="$service_dir/state"
[ -n "$helper_socket" ] || helper_socket="$service_dir/linuxpod-helper.sock"
[ -n "$log_root" ] || log_root="$HOME/macvz-cri-i5-test/kubelet-root/linuxpod-logs"
[ -n "$kubelet_pods_dir" ] || kubelet_pods_dir="$HOME/macvz-cri-i5-test/kubelet-root/pods"
[ -n "$volume_allowed" ] || volume_allowed="$HOME/macvz-cri-i5-test"

binary="$live_root/macvz-cri"
pid_file="$service_dir/macvz-cri.pid"
log_file="$service_dir/macvz-cri.log"

mkdir -p "$service_dir" "$state_dir" "$log_root"
if [ "$codesign_binary" = 1 ] || [ "$codesign_binary" = true ]; then
	codesign -f -s - "$binary" >/dev/null 2>&1 || true
fi

ps -axo pid=,command= |
	grep "[m]acvz-cri" |
	grep -- "$socket" |
	awk "{print \$1}" |
	while read -r pid; do
		[ -n "$pid" ] && kill "$pid" 2>/dev/null || true
	done
sleep 2
ps -axo pid=,command= |
	grep "[m]acvz-cri" |
	grep -- "$socket" |
	awk "{print \$1}" |
	while read -r pid; do
		[ -n "$pid" ] && kill -9 "$pid" 2>/dev/null || true
	done

rm -f "$socket"
nohup "$binary" \
	--listen unix://"$socket" \
	--state-dir "$state_dir" \
	--experimental-linuxpod-backend \
	--linuxpod-helper-socket "$helper_socket" \
	--linuxpod-log-root "$log_root" \
	--kubelet-pods-dir "$kubelet_pods_dir" \
	--volume-host-path-allowed "$volume_allowed" \
	--pod-cidr "$pod_cidr" \
	--pod-network-interface "$pod_network_interface" \
	--pod-network-helper-socket "$pod_network_helper_socket" \
	--pod-network-ingress-interface "$pod_network_ingress_interface" \
	--pod-network-enable-forwarding \
	--streaming-addr "$streaming_addr" \
	-v=4 >> "$log_file" 2>&1 &
new_pid=$!
printf "%s\n" "$new_pid" > "$pid_file"

i=0
while [ "$i" -lt "$timeout" ]; do
	i=$((i + 1))
	if [ -S "$socket" ]; then
		printf "macvz-cri restarted pid=%s\n" "$new_pid"
		exit 0
	fi
	if ! kill -0 "$new_pid" 2>/dev/null; then
		printf "macvz-cri exited before readiness; see %s\n" "$log_file" >&2
		tail -n 120 "$log_file" >&2 2>/dev/null || true
		exit 1
	fi
	sleep 0.1
done

printf "macvz-cri socket did not return within %ss; see %s\n" "$timeout" "$log_file" >&2
tail -n 120 "$log_file" >&2 2>/dev/null || true
exit 1
'

arg_live_root="${live_root:-$default_token}"
arg_service_dir="${service_dir:-$default_token}"
arg_socket="${socket:-$default_token}"
arg_state_dir="${state_dir:-$default_token}"
arg_helper_socket="${helper_socket:-$default_token}"
arg_log_root="${log_root:-$default_token}"
arg_kubelet_pods_dir="${kubelet_pods_dir:-$default_token}"
arg_volume_allowed="${volume_allowed:-$default_token}"

if [ -n "$target" ]; then
	printf '%s\n' "$remote_script" | ssh "$target" /bin/sh -s -- \
		"$arg_live_root" "$arg_service_dir" "$arg_socket" "$arg_state_dir" \
		"$arg_helper_socket" "$arg_log_root" "$arg_kubelet_pods_dir" "$arg_volume_allowed" \
		"$pod_cidr" "$pod_network_interface" "$pod_network_helper_socket" \
		"$pod_network_ingress_interface" "$streaming_addr" "$codesign_binary" "$timeout"
else
	printf '%s\n' "$remote_script" | /bin/sh -s -- \
		"$arg_live_root" "$arg_service_dir" "$arg_socket" "$arg_state_dir" \
		"$arg_helper_socket" "$arg_log_root" "$arg_kubelet_pods_dir" "$arg_volume_allowed" \
		"$pod_cidr" "$pod_network_interface" "$pod_network_helper_socket" \
		"$pod_network_ingress_interface" "$streaming_addr" "$codesign_binary" "$timeout"
fi
