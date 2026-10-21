#!/bin/bash
# Phase 5 e2e: hardening. Units are sandboxed, the container is confined
# (device policy, seccomp, capabilities) and the control service rejects
# forged or malicious requests.
. "$(dirname "$0")/lib.sh"
CG=/sys/fs/cgroup/machine.slice/lapis-android.service/payload
call() { python3 /tmp/control_call.py "$1"; }
as_bob() { sudo -u bob python3 /tmp/control_call.py "$1"; }

step "setup"
id bob >/dev/null 2>&1 || sudo useradd -m bob
session_stop 2>/dev/null; sleep 2
session_start
wait_for 300 "Android booted (hardened units)" booted

step "unit sandboxing"
check "container DevicePolicy=closed" sh -c '[ "$(systemctl show -P DevicePolicy lapis-android)" = closed ]'
check "network unit ProtectSystem=strict" sh -c '[ "$(systemctl show -P ProtectSystem lapis-net)" = strict ]'
check "network unit capability set" sh -c 'systemctl show -P CapabilityBoundingSet lapis-net | grep -qv cap_sys_admin'
check "control unit ProtectSystem=full" sh -c '[ "$(systemctl show -P ProtectSystem lapis-control@x)" = full ]'

step "container confinement"
check "Android init under seccomp" sh -c 'sudo lapis shell cat /proc/1/status | grep -qP "^Seccomp:\t2"'
capbnd=$(sudo lapis shell cat /proc/1/status | awk "/^CapBnd/ {print \$2}")
bit() { [ $(( (0x$capbnd >> $1) & 1 )) -eq "$2" ]; }
check "no CAP_SYS_MODULE" bit 16 0
check "no CAP_SYS_RAWIO" bit 17 0
check "keeps CAP_SYS_ADMIN (Android needs it)" bit 21 1
check "keeps CAP_NET_ADMIN" bit 12 1
check "attached commands in container cgroup" sh -c 'sudo lapis shell cat /proc/self/cgroup | grep -q "^0::/$"'
check "attached commands under seccomp" sh -c 'sudo lapis shell cat /proc/self/status | grep -qP "^Seccomp:\t2"'
check "host disk blocked by device policy" sh -c "! sudo sh -c 'echo \$\$ > $CG/cgroup.procs; head -c1 /dev/vda >/dev/null'"
check "urandom allowed by device policy" sh -c "sudo sh -c 'echo \$\$ > $CG/cgroup.procs; head -c1 /dev/urandom >/dev/null'"
check "module loading refused inside" sh -c '! sudo lapis shell insmod /system/lib/modules/none.ko 2>/dev/null'

step "control service input validation"
# reply_has PATTERN JSON [user]: the control service answers JSON with PATTERN.
reply_has() {
  if [ -n "${3:-}" ]; then out=$(sudo -u "$3" python3 /tmp/control_call.py "$2"); else out=$(call "$2"); fi
  echo "$out" | grep -qE "$1"
}
BOB=$(id -u bob)
check "uid spoofing rejected" reply_has 'on behalf of another user' '{"op":"start","session":{"uid":1000,"data_dir":"/home/arch"}}' bob
check "foreign data dir rejected" reply_has 'not owned|already used' "{\"op\":\"start\",\"session\":{\"uid\":$BOB,\"xdg_runtime_dir\":\"/run/user/1000\",\"wayland_display\":\"$WAYLAND_DISPLAY\",\"data_dir\":\"/home/arch/.local/share/lapis/data\"}}" bob
check "bad package name rejected" reply_has 'invalid package name' '{"op":"launch","package":"x;reboot"}'
check "bad property rejected" reply_has 'invalid property name' '{"op":"getprop","prop":"a b"}'
check "bad adb key rejected" reply_has 'invalid adb public key' '{"op":"adb-key","key":"not a key"}'
check "unknown op rejected" reply_has 'unknown operation' '{"op":"rm-rf"}'
call 'garbage' >/dev/null 2>&1
check "service survives garbage" reply_has RUNNING '{"op":"status"}'
check "other user blocked from apps list" reply_has 'permission denied' '{"op":"apps"}' bob

step "network isolation"
check "NAT rule only in lapis tables" sh -c 'sudo nft list ruleset | grep -B20 masquerade | grep -q "table ip lapis"'
check "container cannot see host interfaces" sh -c '! sudo lapis shell ip link | grep -q "enp\|wlan\|ens"'

session_stop
wait_for 60 "stopped" state_is STOPPED
summary
