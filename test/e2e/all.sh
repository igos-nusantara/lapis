#!/bin/sh
# Full end-to-end regression: every phase against the current tree.
# Phase 4 replaces the development install with packages, so it runs last
# and the development install is restored afterwards.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$HERE/../..
LOG=$HERE/work
fail=0
"$HERE/deploy.sh" || exit 1
restart_vm() { # phase3 suspends the guest; start the next phase on a fresh boot
  "$HERE/vm.sh" stop
  "$HERE/vm.sh" start >/dev/null
  for _ in $(seq 60); do "$HERE/vm.sh" ssh true 2>/dev/null && return 0; sleep 5; done
  return 1
}
for p in phase1 phase2 phase3 phase5; do
  "$HERE/run.sh" $p > "$LOG/$p.log" 2>&1 || fail=1
  printf '%-8s %s\n' "$p" "$(tail -1 "$LOG/$p.log")"
  [ $p = phase3 ] && restart_vm
done
(cd "$ROOT" && PATH=$HOME/.local/go/bin:$PATH VERSION=${VERSION:-0.1.0} scripts/release.sh >/dev/null 2>&1) || fail=1
"$HERE/run.sh" phase4 > "$LOG/phase4.log" 2>&1 || fail=1
printf '%-8s %s\n' phase4 "$(tail -1 "$LOG/phase4.log")"
"$HERE/deploy.sh" >/dev/null
exit $fail
