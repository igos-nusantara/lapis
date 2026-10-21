#!/bin/sh
systemctl stop lapis-android.service lapis-net.service lapis-binder.service >/dev/null 2>&1 || true
systemctl disable --now lapis-control.socket >/dev/null 2>&1 || true
