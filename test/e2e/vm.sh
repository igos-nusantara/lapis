#!/bin/sh
# Starts the Arch Linux test VM inside the lapis-vmhost container.
# Usage: test/e2e/vm.sh start|stop|ssh [cmd...]|status
# E2E_GPU=virgl gives the guest a virtio-gpu backed by the host's render
# node (E2E_RENDER_NODE, default /dev/dri/renderD128).
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
WORK=$HERE/work
NAME=lapis-e2e-vm
SSH="ssh -q -i $WORK/id_e2e -p 2222 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 arch@127.0.0.1"

seed() {
  mkdir -p "$WORK/seed"
  cat > "$WORK/seed/meta-data" <<META
instance-id: lapis-e2e
local-hostname: lapis-e2e
META
  cat > "$WORK/seed/user-data" <<USER
#cloud-config
users:
  - name: arch
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - $(cat "$WORK/id_e2e.pub")
USER
}

case "${1:-}" in
start)
  [ -f "$WORK/disk.qcow2" ] || {
    cp "$WORK/arch.qcow2" "$WORK/disk.qcow2"
    podman run --rm -v "$WORK:/work:z" localhost/lapis-vmhost qemu-img resize /work/disk.qcow2 30G
  }
  seed
  podman rm -f "$NAME" >/dev/null 2>&1 || true
  gpu_dev= display="-display none"
  if [ "${E2E_GPU:-}" = virgl ]; then
    node=${E2E_RENDER_NODE:-/dev/dri/renderD128}
    gpu_dev="--device $node"
    display="-device virtio-gpu-gl-pci -display egl-headless,rendernode=$node"
  fi
  podman run -d --name "$NAME" --device /dev/kvm $gpu_dev -e DISPLAY_ARGS="$display" -p 127.0.0.1:2222:2222 \
    -v "$WORK:/work:z" localhost/lapis-vmhost sh -c '
      mkisofs -quiet -o /work/seed.iso -V cidata -J -r /work/seed &&
      exec qemu-system-x86_64 -enable-kvm -cpu host -machine q35 -smp 4 -m 6G \
        -drive file=/work/disk.qcow2,if=virtio,discard=unmap \
        -drive file=/work/seed.iso,if=virtio,format=raw,readonly=on \
        -nic user,model=virtio-net-pci,hostfwd=tcp::2222-:22 \
        $DISPLAY_ARGS -serial file:/work/serial.log -monitor none'
  ;;
stop)
  # Clean shutdown, so the guest flushes its disk before QEMU exits.
  $SSH 'sudo systemctl poweroff' >/dev/null 2>&1 || true
  for _ in $(seq 60); do [ -z "$(podman ps -q -f name=$NAME)" ] && break; sleep 1; done
  podman rm -f "$NAME" >/dev/null 2>&1 || true ;;
status) podman ps --filter name="$NAME" ;;
ssh) shift; exec $SSH "$@" ;;
*) echo "usage: $0 start|stop|ssh|status" >&2; exit 2 ;;
esac
