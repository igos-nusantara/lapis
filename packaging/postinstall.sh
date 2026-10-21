#!/bin/sh
systemctl daemon-reload >/dev/null 2>&1 || true
systemctl enable --now lapis-control.socket >/dev/null 2>&1 || true
