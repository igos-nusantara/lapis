#!/bin/bash
# GPU e2e: needs a VM started with E2E_GPU=virgl (test/e2e/vm.sh). Android
# must render through the host GPU, and a GL game must reach a smooth frame
# rate in its own window. Run with: test/e2e/run.sh phase-gpu
. "$(dirname "$0")/lib.sh"
DATA=~/.local/share/lapis/data
PKG=io.anuke.mindustry
APK=mindustry.apk

# SurfaceFlinger's frame counter (transaction 1013).
sf_frames() { printf '%d' "0x$(sudo lapis shell service call SurfaceFlinger 1013 | grep -oE '\(([0-9a-f]{8})' | tr -d '(')"; }
fps_at_least() {
  a=$(sf_frames); sleep 5; b=$(sf_frames)
  echo "fps: $(( (b - a) / 5 ))"
  [ $(( (b - a) / 5 )) -ge "$1" ]
}
gles() { sudo lapis shell dumpsys SurfaceFlinger | awk '/^GLES:/ && !s {print; s=1}'; }

step "setup"
check "host render node" sh -c 'ls /dev/dri/renderD*'
session_start
wait_for 60 "container running" state_is RUNNING
wait_for 420 "Android booted" booted

step "hardware rendering"
gles | sed 's/^/    /'
check "gralloc uses the render node" sh -c '[ "$(lapis prop get ro.hardware.egl)" = mesa ]'
check "SurfaceFlinger on the GPU" sh -c "$(declare -f gles); gles | grep -v -i -E 'llvmpipe|softpipe|swiftshader' | grep -q GLES"

step "GL game"
[ -s /tmp/$APK ] || curl -sfL -o /tmp/$APK "https://f-droid.org/repo/io.anuke.mindustry_1115.apk"
sudo install -m 0644 /tmp/$APK "$DATA/local/tmp/$APK"
sudo lapis shell pm uninstall $PKG >/dev/null 2>&1
check "install $PKG" sh -c "sudo lapis shell pm install /data/local/tmp/$APK | grep -q Success"
check "launch" lapis app launch $PKG
wait_for 60 "game window on host" has_window_for $PKG
sleep 20
check "game process alive" sudo lapis shell pidof $PKG
check "at least 30 fps" fps_at_least 30
mkdir -p ~/shots && grim ~/shots/gpu-mindustry.png
check "uninstall" sh -c "sudo lapis shell pm uninstall $PKG | grep -q Success"

session_stop
summary
