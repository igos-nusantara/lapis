#!/bin/bash
# Phase 1 e2e: the container boots the GAPPS image and the host-side
# contract (network, Wayland, audio, apps, lifecycle) works.
. "$(dirname "$0")/lib.sh"

step "boot"
session_start
wait_for 60 "container running" state_is RUNNING
wait_for 420 "Android boot completed" booted || {
  sudo journalctl -u lapis-android -n 40 --no-pager
  sudo lapis logcat -d -b crash 2>/dev/null | tail -30
}

step "kernel interfaces"
check "binder nodes inside" adroid ls /dev/binder /dev/vndbinder /dev/hwbinder
check "servicemanager running" adroid pidof servicemanager
check "surfaceflinger running" adroid pidof surfaceflinger

step "network"
check "eth0 has 192.168.240.112" sh -c 'sudo lapis shell ip -4 addr show eth0 | grep -q 192.168.240.112'
check "host reachable" adroid ping -c 1 -W 5 192.168.240.1
check "DNS + internet" adroid ping -c 1 -W 5 dns.google

step "display and audio"
check "composer HAL running (windows below prove Wayland)" sh -c 'sudo lapis shell ps -A | grep -q graphics.composer'
check "composer hook preloaded" sh -c 'sudo lapis shell sh -c "grep -q liblapis_hwc /proc/\$(pidof android.hardware.graphics.composer@2.1-service)/maps"'
check "shell has Android environment" sh -c 'sudo lapis shell sh -c "echo \$EXTERNAL_STORAGE" | grep -q /sdcard'
check "pulse socket inside" adroid ls /run/xdg/pulse/native

step "apps"
check "Play Store installed" sh -c 'lapis app list | grep -q com.android.vending'
check "Google Play services installed" adroid pm path com.google.android.gms
check "launch browser" lapis app launch org.lineageos.jelly
wait_for 30 "browser resumed" sh -c 'sudo lapis shell dumpsys activity activities | grep -E "(mResumedActivity|topResumedActivity).*org.lineageos.jelly"'
wait_for 30 "browser has its own host window" has_windows
check "desktop entries written" sh -c 'ls ~/.local/share/applications/lapis.*.desktop | grep -q vending'

step "lifecycle"
check "freeze" lapis freeze
check "state FROZEN" state_is FROZEN
check "unfreeze" lapis unfreeze
check "state RUNNING" state_is RUNNING
check "gapps-id readable" sh -c 'for i in $(seq 1 40); do sudo lapis gapps-id 2>/dev/null | head -1 | grep -qE "^[0-9a-f]+$" && exit 0; sleep 5; done; exit 1'

session_stop
wait_for 60 "container stopped" state_is STOPPED
check "rootfs unmounted" sh -c '! findmnt /var/lib/lapis/rootfs'
check "veth removed" sh -c '! ip link show lapis0'
check "nft tables removed" sh -c '! sudo nft list tables | grep -q lapis'

step "restart"
session_start
wait_for 300 "boots again" booted
session_stop
wait_for 60 "stopped again" state_is STOPPED

summary
