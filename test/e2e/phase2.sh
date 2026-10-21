#!/bin/bash
# Phase 2 e2e: host binder services in pure Go (IPlatform client,
# IUserMonitor, IClipboard, IHardware, INotifications).
. "$(dirname "$0")/lib.sh"
APPS=~/.local/share/applications
NOTIFY_LOG=/tmp/notify.log

step "setup"
rm -f "$NOTIFY_LOG"
systemctl --user stop fake-notify 2>/dev/null; systemctl --user reset-failed fake-notify 2>/dev/null
systemd-run --user --unit=fake-notify python3 /tmp/fake_notify.py "$NOTIFY_LOG" >/dev/null
session_stop 2>/dev/null; sleep 2
rm -f $APPS/lapis.*.desktop
session_start
wait_for 300 "Android booted" booted

step "service registration"
wait_for 30 "host services visible to Android" sh -c 'sudo lapis shell service list | grep -q usermonitor'
for s in clipboard hardware notifications; do
  check "$s service registered" sh -c "lapis debug services | grep -q '$s\$'"
done

step "IPlatform client"
check "getprop over binder" sh -c '[ "$(lapis debug call platform IPlatform 1 s:ro.build.version.release s:x "->" s)" = 13 ]'
check "real app labels" sh -c 'lapis app list | grep -q "com.android.vending .*Google Play Store"'

step "IUserMonitor"
wait_for 30 "userUnlocked received" sh -c 'journalctl --user -u lapis-session | grep -q "user unlocked"'
wait_for 30 "desktop entry uses label" grep -qx "Name=Browser" $APPS/lapis.org.lineageos.jelly.desktop
check "settings action in entry" grep -q "APPLICATION_DETAILS_SETTINGS package:org.lineageos.jelly" $APPS/lapis.org.lineageos.jelly.desktop
check "disable app" adroid pm disable-user --user 0 org.lineageos.recorder
wait_for 90 "entry removed when app is gone" sh -c "! test -e $APPS/lapis.org.lineageos.recorder.desktop"
check "re-enable app" adroid pm enable --user 0 org.lineageos.recorder
wait_for 90 "entry added back" test -e $APPS/lapis.org.lineageos.recorder.desktop

step "launch over binder (no root)"
check "app launch" lapis app launch org.lineageos.eleven
wait_for 30 "Music window on host" has_window_for org.lineageos.eleven
check "app intent (settings page)" lapis app intent android.settings.APPLICATION_DETAILS_SETTINGS package:org.lineageos.jelly
wait_for 30 "Settings window on host" has_window_for com.android.settings
if ! has_window_for com.android.settings; then
  echo "    windows: $(android_windows | tr '\n' ' ')"
  echo "    active: $(lapis prop get $(lapis debug app-id x | cut -d. -f1).active_apps)"
  sudo lapis shell dumpsys activity activities | grep -E "topResumedActivity|ResumedActivity" | head -3 | sed 's/^/    /'
  sudo lapis shell am stack list | grep -E "taskId=" | head -5 | sed 's/^/    /'
fi

step "IClipboard"
check "host -> Android" sh -c 'wl-copy lapis-host-clip </dev/null >/dev/null 2>&1 && [ "$(lapis debug call clipboard IClipboard 2 "->" s)" = lapis-host-clip ]'
check "Android -> host" sh -c 'lapis debug call clipboard IClipboard 1 s:lapis-android-clip && sleep 1 && [ "$(wl-paste -n)" = lapis-android-clip ]'

step "INotifications"
# Android forwards app notifications (not ones posted by the shell); the
# setup wizard posts one on every boot of a fresh data dir.
wait_for 60 "notification reached desktop" grep -q '"entry": "lapis.' "$NOTIFY_LOG"
check "notification carries icon" grep -q '"image": true' "$NOTIFY_LOG"

step "IHardware"
check "suspend request" lapis debug call hardware IHardware 3
wait_for 20 "container frozen" state_is FROZEN
check "launch thaws container" lapis app launch org.lineageos.jelly
check "state RUNNING again" state_is RUNNING
check "freeze again" lapis freeze
check "queries fail fast while frozen" sh -c 'timeout 10 lapis prop get sys.boot_completed 2>&1 | grep -q suspended'
check "app list works while frozen" sh -c 'timeout 60 lapis app list | grep -q com.android.settings'
check "app list leaves it frozen" state_is FROZEN
session_stop
wait_for 60 "frozen container stops" state_is STOPPED
session_start
wait_for 300 "boots again after frozen stop" booted
check "reboot request" lapis debug call hardware IHardware 4
sleep 5
wait_for 300 "Android back after reboot" booted
wait_for 60 "services republished after reboot" sh -c 'sudo lapis shell service list | grep -q usermonitor'

session_stop
wait_for 60 "stopped" state_is STOPPED
systemctl --user stop fake-notify
summary
