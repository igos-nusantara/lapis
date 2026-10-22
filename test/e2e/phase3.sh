#!/bin/bash
# Phase 3 e2e: desktop integration (full UI, adb, sleep, multi-user
# isolation) and ARM translation extras.
. "$(dirname "$0")/lib.sh"
DATA=~/.local/share/lapis/data

step "setup"
session_stop 2>/dev/null; sleep 2
for t in libndk libhoudini; do sudo lapis extras remove $t >/dev/null 2>&1; done
session_start
wait_for 300 "Android booted" booted

step "show full UI"
check "show-full-ui" lapis show-full-ui
wait_for 30 "full UI window" has_window_for full-ui

step "display modes"
check "default is tablet, maximized" sh -c 'lapis display | grep -q "mode: tablet" && lapis display | grep -q "windows: maximized"'
check "tablet smallest width >= 600dp" sw_at_least 600
check "open a Settings sub-page first" lapis app intent android.settings.APPLICATION_DETAILS_SETTINGS package:org.lineageos.jelly
sleep 3
check "launch Settings" lapis app launch com.android.settings
wait_for 20 "Settings maximized (tablet layout)" settings_maximized
wait_for 20 "tablet: host window spans the display" window_width_at_least com.android.settings 1200
check "switch to phone" sudo lapis display phone
session_stop; wait_for 60 "stopped" state_is STOPPED
session_start; wait_for 300 "booted (phone)" booted
check "phone smallest width < 600dp" sw_below 600
check "launch Settings (phone)" lapis app launch com.android.settings
sleep 5
check "Settings uses the phone layout (< 600dp)" settings_phone_layout
wait_for 20 "phone: host window fits the app" window_width_below com.android.settings 700
check "Android display keeps the output size" display_size_is 1280x800
check "back to tablet" sudo lapis display tablet
session_stop; wait_for 60 "stopped" state_is STOPPED
session_start; wait_for 300 "booted (tablet)" booted

step "host window drives the app size"
S=$(lapis debug app-id com.android.settings)
check "launch Settings" lapis app launch com.android.settings
sleep 8
swaymsg "[app_id=\"$S\"] resize set 700 500" >/dev/null
wait_for 30 "resized window: app follows (700 wide)" app_follows_window com.android.settings 700
# Tiling: two apps side by side, each gets half the output.
swaymsg 'for_window [app_id=".+"] floating disable' >/dev/null
swaymsg "[app_id=\"$S\"] floating disable" >/dev/null
swaymsg layout splith >/dev/null
check "launch Browser" lapis app launch org.lineageos.jelly
wait_for 30 "tiled: Settings fits its tile" app_follows_window com.android.settings 640
wait_for 30 "tiled: Browser fits its tile" app_follows_window org.lineageos.jelly 640
check "Android display keeps the output size" display_size_is 1280x800
swaymsg 'for_window [app_id=".+"] floating enable' >/dev/null
swaymsg '[app_id=".+"] floating enable' >/dev/null

step "adb"
check "adb connect" sh -c 'lapis adb connect | grep -q "connected to"'
wait_for 30 "adb shell works" sh -c '[ "$(adb -s 192.168.240.112:5555 shell getprop ro.product.device | tr -d "\r")" = "$(lapis prop get ro.product.device)" ]'
adb disconnect >/dev/null 2>&1

step "multi-user isolation"
id bob >/dev/null 2>&1 || sudo useradd -m bob
check "other user cannot stop" sh -c '! sudo -u bob lapis session stop'
check "other user cannot freeze" sh -c '! sudo -u bob lapis freeze'
check "other user cannot take over" sh -c 'sudo -u bob XDG_RUNTIME_DIR=/tmp lapis session start 2>&1 | grep -qiE "XDG_RUNTIME_DIR|Wayland|another"'
check "container still running" state_is RUNNING

step "BPF maps (no crash on app removal)"
check "Android has /sys/fs/bpf" sh -c 'sudo lapis shell mount | grep -q "on /sys/fs/bpf type bpf"'
check "app removal keeps system_server alive" app_removal_no_crash

step "ARM translation (libndk)"
# Termux publishes per-ABI builds; the arm64 one has no x86 native code.
APK=termux-arm64.apk
[ -s /tmp/$APK ] || curl -sL -o /tmp/$APK "https://github.com/termux/termux-app/releases/download/v0.118.1/termux-app_v0.118.1+github-debug_arm64-v8a.apk"
sudo install -m 0644 /tmp/$APK "$DATA/local/tmp/$APK"
# Start from a known state even if an earlier run was interrupted.
sudo lapis shell pm uninstall com.termux >/dev/null 2>&1
check "ARM-only app refused without translation" sh -c "sudo lapis shell pm install /data/local/tmp/$APK 2>&1 | grep -q NO_MATCHING_ABIS"
check "install libndk" sudo lapis extras install libndk
check "listed as installed" sh -c 'lapis extras list | grep -q "^\* libndk"'
session_stop; wait_for 60 "stopped" state_is STOPPED
session_start
wait_for 300 "booted with libndk" booted
check "native bridge property" sh -c '[ "$(lapis prop get ro.dalvik.vm.native.bridge)" = libndk_translation.so ]'
check "arm64 ABI advertised" sh -c 'lapis prop get ro.product.cpu.abilist | grep -q arm64-v8a'
check "library in /system" adroid ls /system/lib64/libndk_translation.so
check "ARM-only app installs" sh -c "sudo lapis shell pm install /data/local/tmp/$APK | grep -q Success"
check "installed as arm64" sh -c 'sudo lapis shell dumpsys package com.termux | grep -q "primaryCpuAbi=arm64-v8a"'
check "launch ARM app" lapis app launch com.termux
wait_for 60 "ARM app window on host" has_window_for com.termux
check "ARM app process alive" sh -c 'sleep 10; sudo lapis shell pidof com.termux'
check "no crash" sh -c '! sudo lapis logcat -d -b crash | grep -q com.termux'
check "uninstall ARM app" adroid pm uninstall com.termux
check "remove libndk" sudo lapis extras remove libndk
check "property removed from config" sh -c '! sudo grep -q native.bridge /var/lib/lapis/lapis.toml'

# Last: the QEMU guest tends to reset a few minutes after an S3 resume, so
# all.sh restarts the VM after this phase.
step "host sleep"
# Suspend through logind (as desktops do) so PrepareForSleep is sent; the
# RTC alarm wakes the VM up again.
sudo rm -f /etc/systemd/sleep.conf.d/e2e.conf
check "arm RTC wake alarm" sudo rtcwake -m no -s 10
check "systemctl suspend" sudo systemctl suspend
sleep 20
check "froze before sleep" sh -c 'journalctl --user -u lapis-session --since "-2min" | grep -q "freezing Android"'
check "thawed after resume" sh -c 'journalctl --user -u lapis-session --since "-2min" | grep -q "thawing Android"'
check "running after resume" state_is RUNNING

session_stop
wait_for 60 "stopped" state_is STOPPED
summary
