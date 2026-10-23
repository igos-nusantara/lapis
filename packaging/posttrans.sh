#!/bin/sh
# rpm runs the replaced version's preremove after this version's
# postinstall; up to 0.5.3 that script disabled the control socket on
# upgrades too. Enable it again once the transaction is done.
systemctl enable --now lapis-control.socket >/dev/null 2>&1 || true
