#!/bin/sh
# Builds release artifacts into dist/:
#   lapis-$VERSION-linux-$ARCH.tar.gz   static binary + units
#   lapis_$VERSION_$ARCH.{deb,rpm,pkg.tar.zst}
#   lapis-sysext-$VERSION-$ARCH.tar.gz  systemd-sysext tree for /var/lib/extensions
#   SHA256SUMS
set -eu
cd "$(dirname "$0")/.."
VERSION=${VERSION:-$(date +%Y.%m.%d)}
NFPM=${NFPM:-github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.43.0}
rm -rf dist && mkdir -p dist

for GOARCH in amd64 arm64; do
  stage=dist/stage-$GOARCH
  mkdir -p "$stage/usr/bin"
  CGO_ENABLED=0 GOOS=linux GOARCH=$GOARCH go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" -o "$stage/usr/bin/lapis" ./cmd/lapis
  # Units are rendered by the host-arch binary.
  go run ./cmd/lapis units -bin /usr/bin/lapis -out "$stage"

  tar --owner=0 --group=0 --numeric-owner -C "$stage" -czf "dist/lapis-$VERSION-linux-$GOARCH.tar.gz" usr

  # systemd-sysext: same tree plus an extension-release file that matches
  # any OS, so it survives OS updates on image-based systems.
  sysext=dist/sysext-$GOARCH/lapis
  mkdir -p "$sysext/usr/lib/extension-release.d"
  cp -a "$stage/usr" "$sysext/"
  printf 'ID=_any\nARCHITECTURE=%s\n' "$( [ "$GOARCH" = amd64 ] && echo x86-64 || echo arm64 )" \
    > "$sysext/usr/lib/extension-release.d/extension-release.lapis"
  tar --owner=0 --group=0 --numeric-owner -C "dist/sysext-$GOARCH" -czf "dist/lapis-sysext-$VERSION-$GOARCH.tar.gz" lapis

  for pkg in deb rpm archlinux; do
    case "$pkg/$GOARCH" in
      archlinux/amd64) NFPM_ARCH=x86_64 ;; archlinux/arm64) NFPM_ARCH=aarch64 ;;
      *) NFPM_ARCH=$GOARCH ;;
    esac
    sed -e "s/\${GOARCH}/$GOARCH/g" -e "s/\${NFPM_ARCH}/$NFPM_ARCH/g" -e "s/\${VERSION}/$VERSION/g" \
      packaging/nfpm.yaml > "dist/nfpm-$pkg-$GOARCH.yaml"
    go run "$NFPM" package -f "dist/nfpm-$pkg-$GOARCH.yaml" -p "$pkg" -t dist/ >/dev/null
  done
done
rm -rf dist/stage-* dist/sysext-* dist/nfpm-*.yaml
(cd dist && sha256sum -- * > SHA256SUMS)
ls -la dist
