#!/bin/bash
# Third-party app e2e: installs every .apk/.xapk in ~/apps (copy them into
# the VM yourself; nothing is downloaded), then checks that each app opens
# its own window and is still running 30 s later. Screenshots go to ~/shots.
# ARM-only apps need ARM translation: set E2E_TRANSLATION=libndk|libhoudini
# (default libhoudini, which runs more apps than libndk).
# Run with: test/e2e/run.sh phase-apps
. "$(dirname "$0")/lib.sh"
DATA=~/.local/share/lapis/data
APPS=~/apps
TR=${E2E_TRANSLATION:-libhoudini}

# install_splits DIR: installs all APKs in DIR (inside Android) in one session.
install_splits() {
  sudo lapis shell sh -c '
    d=$1; s=$(pm install-create -r | grep -oE "[0-9]+")
    for a in "$d"/*.apk; do
      pm install-write -S $(stat -c %s "$a") "$s" "$(basename "$a")" "$a" >/dev/null || exit 1
    done
    pm install-commit "$s"' sh "$1" | grep -q Success
}

# package_of FILE: package name of an .xapk (manifest.json) or .apk (file name).
package_of() {
  case $1 in
  *.xapk) python3 -c 'import json,sys,zipfile; print(json.load(zipfile.ZipFile(sys.argv[1]).open("manifest.json"))["package_name"])' "$1" ;;
  *) basename "$1" .apk | sed 's/_[0-9]*$//' ;;
  esac
}

step "setup"
shopt -s nullglob
files=("$APPS"/*.apk "$APPS"/*.xapk)
check "apps to test in $APPS" test ${#files[@]} -gt 0
installed_tr=
if ! sudo lapis extras list 2>/dev/null | grep -q "^\* *$TR"; then
  for other in libndk libhoudini; do sudo lapis extras remove $other >/dev/null 2>&1; done
  check "install $TR" sh -c "sudo lapis extras install $TR >/dev/null"
  installed_tr=$TR
fi
session_stop 2>/dev/null; sudo systemctl stop lapis-android 2>/dev/null
session_start
wait_for 60 "container running" state_is RUNNING
wait_for 420 "Android booted" booted
check "ARM ABIs advertised" sh -c 'sudo lapis shell getprop ro.product.cpu.abilist | grep -q arm64-v8a'
mkdir -p ~/shots

for f in "${files[@]}"; do
  pkg=$(package_of "$f")
  step "$pkg"
  dir=/data/local/tmp/apps/$pkg
  sudo rm -rf "$DATA/local/tmp/apps/$pkg" && sudo mkdir -p "$DATA/local/tmp/apps/$pkg"
  case $f in
  *.xapk) sudo python3 -c 'import sys,zipfile; z=zipfile.ZipFile(sys.argv[1]); [z.extract(n, sys.argv[2]) for n in z.namelist() if n.endswith(".apk") and "/" not in n]' "$f" "$DATA/local/tmp/apps/$pkg" ;;
  *) sudo install -m 0644 "$f" "$DATA/local/tmp/apps/$pkg/base.apk" ;;
  esac
  sudo lapis shell pm uninstall "$pkg" >/dev/null 2>&1
  check "install" install_splits "$dir"
  check "launch" lapis app launch "$pkg"
  wait_for 90 "window on host" has_window_for "$pkg"
  sleep 30
  check "still running after 30 s" sudo lapis shell pidof "$pkg"
  grim ~/shots/app-"$pkg".png
done

session_stop
# Leave ARM translation as it was: later phases expect none installed.
[ -n "$installed_tr" ] && sudo lapis extras remove "$installed_tr" >/dev/null
summary
