#!/bin/bash
# Phase 4 e2e: release artifacts install and run (Arch package, systemd-sysext),
# deb/rpm contents are complete.
. "$(dirname "$0")/lib.sh"
D=/tmp/dist

smoke() { # boot Android from the installed artifact and open one window
  session_start
  wait_for 300 "$1: Android booted" booted
  check "$1: app launch" lapis app launch org.lineageos.jelly
  wait_for 30 "$1: window on host" has_window_for org.lineageos.jelly
  session_stop
  wait_for 60 "$1: stopped" state_is STOPPED
}

step "artifacts"
check "checksums" sh -c "cd $D && sha256sum -c --quiet SHA256SUMS"
for f in $D/lapis_*_amd64.deb $D/lapis-*.x86_64.rpm; do
  check "$(basename $f) has binary + units" sh -c "bsdtar -xOf $f 'data.tar*' 2>/dev/null | bsdtar -tf - 2>/dev/null | grep -q usr/lib/systemd/system/lapis-android.service || bsdtar -tf $f | grep -q usr/lib/systemd/system/lapis-android.service"
done
check "tarball files owned by root" sh -c "! tar -tvzf $D/lapis-sysext-*-amd64.tar.gz | grep -v ' root/root \| 0/0 '"
check "arm64 binary is aarch64" sh -c "bsdtar -xOf $D/lapis-*-linux-arm64.tar.gz usr/bin/lapis | file - | grep -q aarch64"

step "remove development install"
session_stop 2>/dev/null
sudo lapis uninstall >/dev/null 2>&1
sudo rm -f /usr/local/bin/lapis
hash -r
check "no lapis left" sh -c '! command -v lapis'

step "Arch package"
check "pacman -U" sudo pacman -U --noconfirm $D/lapis-*-x86_64.pkg.tar.zst
hash -r
check "binary at /usr/bin" sh -c '[ "$(command -v lapis)" = /usr/bin/lapis ]'
check "version stamped" sh -c 'lapis version | grep -q "lapis 0.1.0"'
check "socket enabled by postinstall" systemctl is-active -q lapis-control.socket
systemctl --user daemon-reload
smoke "pkg"
check "pacman -R" sudo pacman -R --noconfirm lapis
check "socket stopped by preremove" sh -c '! systemctl is-active -q lapis-control.socket'

step "systemd-sysext"
sudo mkdir -p /var/lib/extensions
check "unpack sysext" sudo tar -C /var/lib/extensions -xzf $D/lapis-sysext-*-amd64.tar.gz
check "merge" sudo systemd-sysext refresh
hash -r
check "merged binary" sh -c '[ "$(command -v lapis)" = /usr/bin/lapis ] && lapis version | grep -q 0.1.0'
check "sysext listed" sh -c 'systemd-sysext status | grep -q lapis'
sudo systemctl daemon-reload
check "enable socket" sudo systemctl enable --now lapis-control.socket
systemctl --user daemon-reload
smoke "sysext"
sudo systemctl disable --now lapis-control.socket >/dev/null 2>&1
sudo rm -rf /var/lib/extensions/lapis
check "unmerge" sudo systemd-sysext refresh
hash -r
check "gone after unmerge" sh -c '! command -v lapis'

summary
