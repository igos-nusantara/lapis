#!/bin/sh
# Builds lapis and installs it into the test VM.
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$HERE/../..
cd "$ROOT"
PATH=$HOME/.local/go/bin:$PATH CGO_ENABLED=0 go build -trimpath -o bin/lapis ./cmd/lapis
scp -q -i "$HERE/work/id_e2e" -P 2222 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  bin/lapis arch@127.0.0.1:/tmp/lapis
"$HERE/vm.sh" ssh 'sudo /tmp/lapis install >/dev/null && echo installed: $(command -v lapis); systemctl --user daemon-reload'
