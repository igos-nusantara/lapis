#!/bin/bash
# End-to-end run for a shared Ubuntu/Debian server: installs the .deb,
# exercises every phase that is safe on a machine other people use (no
# suspend, no new users, no package manager changes beyond lapis), then
# stops everything. Run with E2E_COMPOSITOR=docker.
. "$(dirname "$0")/lib.sh"
E2E=$(cd "$(dirname "$0")" && pwd)
D=$E2E/dist
DATA=~/.local/share/lapis/data
APPS=~/.local/share/applications
ROOTDEV=$(findmnt -no SOURCE / | xargs -I{} sh -c 'readlink -f {}')

step "install (.deb, phase 4)"
check "apt installs package + systemd-container" sudo apt-get install -y -q "$D"/lapis_*_amd64.deb
check "version" sh -c 'lapis version | grep -q "lapis 0.6.1"'
check "control socket enabled" systemctl is-active -q lapis-control.socket
systemctl --user daemon-reload
lapis doctor | sed 's/^/    /'

step "images"
if ! sudo test -f /var/lib/lapis/images/system.img; then
  check "download GAPPS images" sh -c 'sudo lapis init -type GAPPS >/tmp/init.log 2>&1'
fi
check "images present" sudo test -f /var/lib/lapis/images/vendor.img

step "boot (phase 1)"
session_start
wait_for 60 "container running" state_is RUNNING
wait_for 420 "Android boot completed" booted || sudo journalctl -u lapis-android -n 30 --no-pager
# Nobody touches this headless Android, which would otherwise ask to
# suspend (and get frozen) after its screen timeout.
adroid settings put system screen_off_timeout 1800000 >/dev/null
check "binder nodes inside" adroid ls /dev/binder /dev/vndbinder /dev/hwbinder
check "software rendering on NVIDIA host" sh -c '[ "$(lapis prop get ro.hardware.egl)" != mesa ]'
check "eth0 has 192.168.240.112" sh -c 'sudo lapis shell ip -4 addr show eth0 | grep -q 192.168.240.112'
check "DNS + internet through Docker firewall" adroid ping -c 1 -W 5 dns.google
check "DOCKER-USER rule installed" sudo iptables -C DOCKER-USER -i lapis0 -j ACCEPT
check "composer HAL running (windows below prove Wayland)" sh -c 'sudo lapis shell ps -A | grep -q graphics.composer'

step "binder services and apps (phase 2)"
wait_for 30 "host services visible to Android" sh -c 'sudo lapis shell service list | grep -q usermonitor'
check "Play Store with real label" sh -c 'lapis app list | grep -q "com.android.vending .*Google Play Store"'
wait_for 30 "desktop entry uses label" grep -qx "Name=Browser" $APPS/lapis.org.lineageos.jelly.desktop
check "launch over binder" lapis app launch org.lineageos.jelly
wait_for 60 "Browser window on host" has_window_for org.lineageos.jelly
check "app intent" lapis app intent android.settings.APPLICATION_DETAILS_SETTINGS package:org.lineageos.jelly
wait_for 60 "Settings window on host" has_window_for com.android.settings
check "tablet layout by default (sw >= 600dp)" sw_at_least 600
check "launch Settings" lapis app launch com.android.settings
wait_for 30 "Settings maximized" settings_maximized
wait_for 30 "tablet: host window spans the display" window_width_at_least com.android.settings 1200
check "Android display keeps the output size" display_size_is 1280x800
check "disable app" adroid pm disable-user --user 0 org.lineageos.recorder
wait_for 90 "entry removed when app is gone" sh -c "! test -e $APPS/lapis.org.lineageos.recorder.desktop"
check "re-enable app" adroid pm enable --user 0 org.lineageos.recorder
wait_for 90 "entry added back" test -e $APPS/lapis.org.lineageos.recorder.desktop
check "freeze" lapis freeze
check "state FROZEN" state_is FROZEN
check "launch thaws" lapis app launch org.lineageos.jelly
check "state RUNNING" state_is RUNNING
check "freeze again" lapis freeze
check "queries fail fast while frozen" sh -c 'timeout 10 lapis prop get sys.boot_completed 2>&1 | grep -q suspended'
check "app list works while frozen" sh -c 'timeout 60 lapis app list | grep -q com.android.settings'
check "app list leaves it frozen" state_is FROZEN
session_stop
wait_for 60 "frozen container stops" state_is STOPPED
session_start
wait_for 300 "boots again after frozen stop" booted
check "gapps-id readable" sh -c 'for i in $(seq 1 40); do sudo lapis gapps-id 2>/dev/null | head -1 | grep -qE "^[0-9a-f]+$" && exit 0; sleep 5; done; exit 1'

step "integration and ARM apps (phase 3)"
check "show-full-ui" lapis show-full-ui
wait_for 60 "full UI window" has_window_for full-ui
APK=termux-arm64.apk
[ -s /tmp/$APK ] || curl -sL -o /tmp/$APK "https://github.com/termux/termux-app/releases/download/v0.118.1/termux-app_v0.118.1+github-debug_arm64-v8a.apk"
sudo install -m 0644 /tmp/$APK "$DATA/local/tmp/$APK"
# Start from a known state even if an earlier run was interrupted.
sudo lapis shell pm uninstall com.termux >/dev/null 2>&1
check "ARM-only app refused without translation" sh -c "sudo lapis shell pm install /data/local/tmp/$APK 2>&1 | grep -q NO_MATCHING_ABIS"
check "install libndk" sudo lapis extras install libndk
session_stop; wait_for 60 "stopped" state_is STOPPED
session_start
wait_for 420 "booted with libndk" booted
check "ARM-only app installs" sh -c "sudo lapis shell pm install /data/local/tmp/$APK | grep -q Success"
check "launch ARM app" lapis app launch com.termux
wait_for 60 "ARM app window on host" has_window_for com.termux
check "ARM app alive, no crash" sh -c 'sleep 10; sudo lapis shell pidof com.termux && ! sudo lapis logcat -d -b crash | grep -q com.termux'
adroid pm uninstall com.termux >/dev/null
check "remove libndk" sudo lapis extras remove libndk

step "BPF maps (no crash on app removal)"
check "Android has /sys/fs/bpf" sh -c 'sudo lapis shell mount | grep -q "on /sys/fs/bpf type bpf"'
check "app removal keeps system_server alive" app_removal_no_crash

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

step "hardening (phase 5)"
CG=/sys/fs/cgroup/machine.slice/lapis-android.service/payload
check "DevicePolicy=closed" sh -c '[ "$(systemctl show -P DevicePolicy lapis-android)" = closed ]'
check "Android init under seccomp" sh -c 'sudo lapis shell cat /proc/1/status | grep -qP "^Seccomp:\t2"'
capbnd=$(sudo lapis shell cat /proc/1/status | awk '/^CapBnd/ {print $2}')
check "no CAP_SYS_MODULE" test $(( (0x$capbnd >> 16) & 1 )) -eq 0
check "attached commands confined" sh -c 'sudo lapis shell cat /proc/self/cgroup | grep -q "^0::/$"'
check "host root disk blocked ($ROOTDEV)" sh -c "! sudo sh -c 'echo \$\$ > $CG/cgroup.procs; head -c1 $ROOTDEV >/dev/null'"
call() { python3 $E2E/control_call.py "$1"; }
check "bad package name rejected" sh -c "python3 $E2E/control_call.py '{\"op\":\"launch\",\"package\":\"x;reboot\"}' | grep -q 'invalid package name'"
check "uid spoofing rejected" sh -c "python3 $E2E/control_call.py '{\"op\":\"start\",\"session\":{\"uid\":0,\"data_dir\":\"/root\"}}' | grep -qE 'another user|already used'"

step "Android-initiated reboot and stop"
check "reboot request" lapis debug call hardware IHardware 4
sleep 5
wait_for 420 "Android back after reboot" booted
wait_for 60 "services republished" sh -c 'sudo lapis shell service list | grep -q usermonitor'
session_stop
wait_for 60 "container stopped" state_is STOPPED
check "rootfs unmounted" sh -c '! findmnt /var/lib/lapis/rootfs'
check "veth removed" sh -c '! ip link show lapis0'
check "nft tables removed" sh -c '! sudo nft list tables | grep -q lapis'
check "DOCKER-USER rules removed" sh -c '! sudo iptables -C DOCKER-USER -i lapis0 -j ACCEPT 2>/dev/null'
docker rm -f lapis-e2e-sway >/dev/null 2>&1

summary
