# Shared helpers for e2e scripts; they run inside the test VM as user arch.
set -u
export XDG_RUNTIME_DIR=/run/user/$(id -u)
export WAYLAND_DISPLAY=wayland-0

PASS=0
FAIL=0

ok()   { PASS=$((PASS+1)); printf '  PASS  %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '  FAIL  %s\n' "$*"; }
step() { printf '\n== %s\n' "$*"; }

# check NAME CMD...: passes when CMD succeeds.
check() {
  name=$1; shift
  if out=$("$@" 2>&1); then ok "$name"; else bad "$name: $(echo "$out" | tail -3)"; fi
}

# wait_for SECONDS NAME CMD...: polls CMD until it succeeds.
wait_for() {
  secs=$1; name=$2; shift 2
  end=$(( $(date +%s) + secs ))
  while [ "$(date +%s)" -lt "$end" ]; do
    if "$@" >/dev/null 2>&1; then ok "$name"; return 0; fi
    sleep 3
  done
  bad "$name (timeout ${secs}s)"; return 1
}

# Run a command inside Android.
adroid() { sudo lapis shell "$@"; }

state_is() { lapis status 2>/dev/null | grep -q "container: $1"; }
booted()   { [ "$(lapis prop get sys.boot_completed 2>/dev/null)" = 1 ]; }

# Headless sway stands in for the user's desktop: it has a seat (needed by
# wl-clipboard) and an IPC to inspect windows.
ensure_compositor() {
  if ! systemctl --user is-active -q sway; then
    systemctl --user stop weston 2>/dev/null
    systemctl --user reset-failed sway 2>/dev/null
    mkdir -p ~/.config/sway
    # Floating windows, like the desktops lapis targets (KDE, GNOME).
    printf 'xwayland disable\noutput HEADLESS-1 resolution 1280x800\nfor_window [app_id=".+"] floating enable\n' > ~/.config/sway/config
    # With a GPU (E2E_GPU=virgl) sway must import Android's GPU buffers.
    renderer=pixman
    ls /dev/dri/renderD* >/dev/null 2>&1 && renderer=gles2
    systemd-run --user --unit=sway -E XDG_RUNTIME_DIR="$XDG_RUNTIME_DIR" \
      -E WLR_BACKENDS=headless -E WLR_LIBINPUT_NO_DEVICES=1 -E WLR_RENDERER=$renderer sway >/dev/null
  fi
  # sway picks the first free wayland-N socket; find the one it owns.
  for _ in $(seq 20); do
    pid=$(systemctl --user show -P MainPID sway)
    sock=$(ls "$XDG_RUNTIME_DIR"/sway-ipc.*."$pid".sock 2>/dev/null)
    if [ -n "$sock" ]; then
      WAYLAND_DISPLAY=$(SWAYSOCK=$sock swaymsg -t get_version >/dev/null 2>&1 && \
        cat /proc/"$pid"/environ 2>/dev/null | tr '\0' '\n' | sed -n 's/^WAYLAND_DISPLAY=//p')
      [ -n "$WAYLAND_DISPLAY" ] || WAYLAND_DISPLAY=$(cd "$XDG_RUNTIME_DIR" && ls -t wayland-? | head -1)
      export WAYLAND_DISPLAY SWAYSOCK=$sock
      return 0
    fi
    sleep 0.5
  done
  return 1
}


# app_ids of Android windows mapped by the compositor.
android_windows() {
  swaymsg -t get_tree 2>/dev/null | grep -oE '"app_id": "[^"]+"' | sort -u
}

has_windows() { [ "$(android_windows | wc -l)" -ge "${1:-1}" ]; }
# has_window_for PACKAGE|full-ui
has_window_for() { android_windows | grep -qF "\"$(lapis debug app-id "$1")\""; }

session_start() {
  ensure_compositor
  systemctl --user set-environment XDG_RUNTIME_DIR="$XDG_RUNTIME_DIR" WAYLAND_DISPLAY="$WAYLAND_DISPLAY"
  systemctl --user start lapis-session.service
}
session_stop() { systemctl --user stop lapis-session.service; }

summary() {
  printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
  [ "$FAIL" -eq 0 ]
}

# On hosts without a desktop stack (shared servers), sway runs in Docker as
# the calling user and only its sockets are shared with the host.
if [ "${E2E_COMPOSITOR:-}" = docker ]; then
  SWAY_CTR=lapis-e2e-sway
  ensure_compositor() {
    if [ -z "$(docker ps -q -f name=^$SWAY_CTR$)" ]; then
      docker rm -f $SWAY_CTR >/dev/null 2>&1
      docker run -d --name $SWAY_CTR --user "$(id -u):$(id -g)" \
        -e XDG_RUNTIME_DIR="$XDG_RUNTIME_DIR" -e HOME=/tmp \
        -e WLR_BACKENDS=headless -e WLR_LIBINPUT_NO_DEVICES=1 -e WLR_RENDERER=pixman \
        -v "$XDG_RUNTIME_DIR:$XDG_RUNTIME_DIR" lapis-e2e-sway \
        sh -c 'printf "xwayland disable\noutput HEADLESS-1 resolution 1280x800\nfor_window [app_id=\".+\"] floating enable\n" > /tmp/cfg; exec sway -c /tmp/cfg' >/dev/null
    fi
    for _ in $(seq 40); do
      SWAYSOCK=$(ls -t "$XDG_RUNTIME_DIR"/sway-ipc.*.sock 2>/dev/null | head -1)
      WAYLAND_DISPLAY=$(cd "$XDG_RUNTIME_DIR" && for w in $(ls -t wayland-? 2>/dev/null); do
        python3 -c "import socket,sys; socket.socket(socket.AF_UNIX).connect(sys.argv[1])" "$w" 2>/dev/null && { echo "$w"; break; }
      done)
      if [ -n "$SWAYSOCK" ] && [ -n "$WAYLAND_DISPLAY" ] && swaymsg -t get_version >/dev/null 2>&1; then
        export SWAYSOCK WAYLAND_DISPLAY
        return 0
      fi
      sleep 0.5
    done
    return 1
  }
  swaymsg() { docker exec $SWAY_CTR swaymsg -s "$SWAYSOCK" "$@"; }
fi

# Smallest display width in dp, from the display size and density.
smallest_width_dp() {
  # wm occasionally prints nothing while Android is busy; retry.
  for _ in 1 2 3 4 5; do
    _sw_once && return 0
    sleep 2
  done
  return 1
}
_sw_once() {
  size=$(sudo lapis shell wm size | awk -F': ' '/Override size/ {o=$2} /Physical size/ {p=$2} END {print (o ? o : p)}' | tr -d '\r')
  dpi=$(sudo lapis shell wm density | awk -F': ' '/Override density/ {o=$2} /Physical density/ {p=$2} END {print (o ? o : p)}' | tr -d '\r')
  [ -n "$size" ] && [ -n "$dpi" ] || return 1
  w=${size%x*}; h=${size#*x}; [ "$w" -lt "$h" ] && h=$w
  echo $(( h * 160 / dpi ))
}
sw_at_least() { v=$(smallest_width_dp) && [ "$v" -ge "$1" ]; }
sw_below() { v=$(smallest_width_dp) && [ "$v" -lt "$1" ]; }

# Left edge of the newest visible Settings task (0 when maximized).
settings_left() {
  sudo lapis shell am stack list | grep -E "taskId=[0-9]+: com.android.settings/.*visible=true" \
    | sed -E 's/.*taskId=([0-9]+):.*bounds=\[([0-9]+),.*/\1 \2/' | sort -n | tail -1 | cut -d" " -f2
}
settings_maximized() { [ "$(settings_left)" = 0 ]; }
settings_not_maximized() { l=$(settings_left); [ -n "$l" ] && [ "$l" != 0 ]; }

# uninstall_stable PKG: uninstall for user 0 until it stays uninstalled.
# Right after boot Android's second user unlock reinstalls system apps,
# silently undoing an earlier uninstall.
uninstall_stable() {
  for _ in $(seq 9); do
    sudo lapis shell pm uninstall --user 0 "$1" >/dev/null 2>&1
    sleep 10
    sudo lapis shell pm list packages --user 0 | grep -qx "package:$1" || return 0
  done
  return 1
}

# app_removal_no_crash: install and uninstall a regular app (UID_REMOVED)
# and check system_server did not crash, which it does without BPF maps.
app_removal_no_crash() {
  local data=~/.local/share/lapis/data
  [ -s /tmp/fdroid.apk ] || curl -sL -o /tmp/fdroid.apk https://f-droid.org/F-Droid.apk
  sudo install -m 0644 /tmp/fdroid.apk "$data/local/tmp/fdroid.apk"
  sudo lapis shell pm install /data/local/tmp/fdroid.apk | grep -q Success || return 1
  local before; before=$(sudo ls "$data/system/dropbox" | grep -c system_server_crash)
  sudo lapis shell pm uninstall org.fdroid.fdroid | grep -q Success || return 1
  sleep 10
  [ "$(sudo ls "$data/system/dropbox" | grep -c system_server_crash)" -le "$before" ]
}

# Smallest width (dp) of the newest visible task of PKG, from am stack list.
task_sw() {
  sudo lapis shell am stack list | awk -v pkg="$1/" '
    /configuration=/ { if (match($0, /sw[0-9]+dp/)) sw = substr($0, RSTART+2, RLENGTH-4) }
    index($0, "taskId=") && index($0, pkg) && /visible=true/ { last = sw }
    END { print last }'
}
settings_phone_layout() { v=$(task_sw com.android.settings) && [ -n "$v" ] && [ "$v" -lt 600 ]; }

# window_size PKG|full-ui: "WxH" of the host window, from the compositor.
window_size() {
  swaymsg -t get_tree | python3 -c '
import json, sys
want = sys.argv[1]
def walk(n):
    if n.get("app_id") == want:
        r = n["rect"]
        print("%dx%d" % (r["width"], r["height"])); sys.exit(0)
    for k in ("nodes", "floating_nodes"):
        for c in n.get(k, []): walk(c)
walk(json.load(sys.stdin))' "$(lapis debug app-id "$1")"
}

# window_width_below PKG N / window_width_at_least PKG N
window_width_below() { w=$(window_size "$1") && [ -n "$w" ] && [ "${w%x*}" -lt "$2" ]; }
window_width_at_least() { w=$(window_size "$1") && [ -n "$w" ] && [ "${w%x*}" -ge "$2" ]; }
display_size_is() { [ "$(sudo lapis shell wm size | awk -F': ' '/Physical size/ {print $2}' | tr -d '\r')" = "$1" ]; }

# task_width PKG: width of the newest visible task of PKG.
task_width() {
  sudo lapis shell am stack list | grep -E "taskId=[0-9]+: $1/.*visible=true" \
    | sed -E 's/.*bounds=\[([0-9]+),[0-9]+\]\[([0-9]+),.*/\1 \2/' | tail -1 | awk '{print $2-$1}'
}
# near A B TOL: |A-B| <= TOL
near() { [ -n "$1" ] && [ $(( $1 > $2 ? $1 - $2 : $2 - $1 )) -le "$3" ]; }
window_width() { w=$(window_size "$1"); echo "${w%x*}"; }
app_follows_window() { near "$(task_width "$1")" "$2" 12 && near "$(window_width "$1")" "$2" 12; }
