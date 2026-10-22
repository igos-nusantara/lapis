#!/bin/sh
# Rebuilds liblapis_hwc-<arch>.so from hook.c with clang and lld (run via
# `go generate ./internal/hwchook`). No NDK is needed: the library only
# imports dladdr/dlopen/dlsym, which link against a stub libdl.so here and
# resolve to Android's libdl at run time.
set -eu
cd "$(dirname "$0")"
if ! command -v clang >/dev/null || ! command -v ld.lld >/dev/null; then
  exec podman run --rm -v "$PWD:/src:z" -w /src docker.io/library/archlinux:latest \
    sh -c 'pacman -Sy --noconfirm --needed clang lld >/dev/null && sh build.sh'
fi
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
printf 'void dladdr(void){}\nvoid dlopen(void){}\nvoid dlsym(void){}\n' > "$tmp/stub.c"
for arch in x86_64 aarch64; do
  target=$arch-linux-android29
  clang --target=$target -fPIC -shared -nostdlib -fuse-ld=lld \
    -Wl,-soname,libdl.so -o "$tmp/libdl.so" "$tmp/stub.c"
  clang --target=$target -O2 -fPIC -shared -nostdlib -ffreestanding -fno-builtin \
    -fno-stack-protector -fvisibility=default -fuse-ld=lld \
    -Wl,-soname,liblapis_hwc.so -Wl,-z,now -Wl,-z,relro -Wl,--hash-style=both \
    -Wl,--build-id=none -L"$tmp" -ldl -o liblapis_hwc-$arch.so hook.c
done
