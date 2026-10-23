#!/bin/sh
# Stop lapis only when the package is removed, not when an upgrade replaces
# it: rpm passes the number of versions left (0 on erase), deb passes
# "upgrade", and pacman runs this on removal only.
case "$1" in
	upgrade | failed-upgrade) exit 0 ;;
	"" | 0 | *[!0-9]*) ;;
	*) exit 0 ;;
esac
systemctl stop lapis-android.service lapis-net.service lapis-binder.service >/dev/null 2>&1 || true
systemctl disable --now lapis-control.socket >/dev/null 2>&1 || true
