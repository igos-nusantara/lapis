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
# window_has_content PACKAGE: the host window shows more than a few colors
# below its title bar (a black SurfaceView hole has one).
window_has_content() {
  rect=$(swaymsg -t get_tree | python3 -c '
import json, sys
app = sys.argv[1]
def walk(n):
    if n.get("app_id", "").endswith(app):
        r = n["rect"]; print("%d,%d %dx%d" % (r["x"], r["y"] + 60, r["width"], r["height"] - 120)); sys.exit()
    for c in n.get("nodes", []) + n.get("floating_nodes", []): walk(c)
walk(json.load(sys.stdin))' "$1")
  [ -n "$rect" ] || return 1
  grim -g "$rect" -t ppm - | python3 -c '
import sys
d = sys.stdin.buffer.read()
parts = d.split(b"\n", 3)
px = parts[3]
colors = {px[i:i+3] for i in range(0, len(px), 3 * 97)}
sys.exit(0 if len(colors) > 20 else 1)'
}
# untagged_surface PACKAGE: a BLAST layer of PACKAGE whose name lacks "TID:".
untagged_surface() { sudo lapis shell dumpsys SurfaceFlinger | grep -q "^ $1/.*(BLAST)#"; }
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

step "SurfaceView game in its own window"
# SuperTuxKart (SDL) renders into a SurfaceView created with the
# Application context; without the composer hook its window stays black.
STK=org.supertuxkart.stk
[ -s /tmp/stk.apk ] || curl -sfL -o /tmp/stk.apk "https://f-droid.org/repo/org.supertuxkart.stk_312.apk"
sudo install -m 0644 /tmp/stk.apk "$DATA/local/tmp/stk.apk"
sudo lapis shell pm uninstall $STK >/dev/null 2>&1
check "install $STK" sh -c "sudo lapis shell pm install /data/local/tmp/stk.apk | grep -q Success"
check "launch" lapis app launch $STK
wait_for 60 "game window on host" has_window_for $STK
# First start extracts the game data in an ordinary dialog; the check only
# means something once the game draws into its untagged SurfaceView.
wait_for 300 "game surface without task prefix" untagged_surface $STK
sleep 15
wait_for 60 "game draws in its window" window_has_content $STK
grim ~/shots/gpu-supertuxkart-window.png
check "uninstall" sh -c "sudo lapis shell pm uninstall $STK | grep -q Success"

session_stop
summary
