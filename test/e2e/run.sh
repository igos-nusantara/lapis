#!/bin/sh
# Copies the e2e scripts into the VM and runs one phase: run.sh phase1
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
scp -q -i "$HERE/work/id_e2e" -P 2222 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  "$HERE"/lib.sh "$HERE"/phase*.sh "$HERE"/*.py arch@127.0.0.1:/tmp/
if [ -d "$HERE/../../dist" ]; then
  "$HERE/vm.sh" ssh 'rm -rf /tmp/dist'
  scp -q -r -i "$HERE/work/id_e2e" -P 2222 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    "$HERE/../../dist" arch@127.0.0.1:/tmp/dist
fi
exec "$HERE/vm.sh" ssh "bash /tmp/$1.sh"
