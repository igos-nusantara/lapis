# AGENTS.md

Guidance for coding agents (and humans) working on lapis. Read
[README.md](README.md) for what lapis is and [PLAN.md](PLAN.md) for the
design, findings and verification history.

## What this is

A single static Go binary (`cmd/lapis`) that runs a LineageOS Android image
in a **systemd-nspawn** container on the host kernel, plus the systemd units
it installs. No LXC, no Python, no cgo, no external helpers beyond systemd
and util-linux.

## Layout

| Path | Contents |
|---|---|
| `cmd/lapis/` | CLI entry point and subcommands; `main.version` set via `-ldflags` |
| `internal/compat/` | **the only place** for the Android image's protocol names (property namespace, binder service/interface names, OTA URLs) |
| `internal/container/` | systemd-nspawn invocation, `attach-exec`, PID-1 shim |
| `internal/control/` | privileged, socket-activated control service (SO_PEERCRED auth): launch, resize, freeze |
| `internal/session/` | unprivileged user session: Wayland proxy, desktop entries, host services loop |
| `internal/wlproxy/` | Wayland proxy that fits app windows and turns host resizes into task resizes |
| `internal/binder/`, `internal/hostsvc/` | pure-Go binder (AIDL) client/server and the host services Android calls |
| `internal/netsvc/` | nftables NAT over netlink, DHCP and DNS |
| `internal/seccomp/` | raw BPF seccomp filter |
| `internal/hwchook/` | composer hook: `csrc/hook.c` (freestanding C, kept out of the Go package) built into `liblapis_hwc-*.so` by `build.sh` (`go generate ./internal/hwchook`; uses clang+lld, or podman when missing); commit the rebuilt `.so` files |
| `internal/units/` | systemd unit templates (`lapis-android`, `-binder`, `-net`, `-control`, `lapis-session`) |
| `internal/config/`, `internal/paths/`, `internal/props/` | `/var/lib/lapis/lapis.toml`, well-known paths, Android property file |
| `internal/images/`, `internal/rootfs/`, `internal/extras/` | image download, rootfs assembly, libndk/libhoudini |
| `internal/doctor/`, `internal/drivers/`, `internal/gpu/`, `internal/sysutil/` | host checks and helpers |
| `packaging/nfpm.yaml` | deb / rpm / Arch package definition |
| `scripts/release.sh` | builds everything into `dist/` |
| `test/e2e/` | end-to-end harness (VM and shared-server) |
| `docs/screenshots/` | screenshots used by the READMEs |

## Build and test

Go version comes from `go.mod`. Before every commit:

```sh
gofmt -l .            # must print nothing
go vet ./...
go test ./...
```

Some unit tests skip themselves without `systemd-analyze` or seccomp support.

Packages (static, amd64 + arm64; nfpm runs via `go run`, override with `NFPM=`):

```sh
VERSION=0.0.0-dev scripts/release.sh   # dist/: tarballs, deb, rpm, pkg.tar.zst, sysext, SHA256SUMS
```

## End-to-end tests

Changes that touch the container, units, binder, networking, the Wayland
proxy or the session **must** pass the e2e suite, not just `go test`.

VM (Arch Linux under QEMU/KVM inside podman; needs `/dev/kvm`):

```sh
test/e2e/vm.sh start          # boots the VM (container lapis-e2e-vm)
test/e2e/deploy.sh            # builds bin/lapis and installs it in the VM
test/e2e/run.sh phase1        # one phase: phase1..phase5
test/e2e/all.sh               # every phase; logs in test/e2e/work/phaseN.log
test/e2e/vm.sh stop
```

GPU: `E2E_GPU=virgl test/e2e/vm.sh start` gives the VM a virtio-gpu backed
by the host's render node (rebuild the `lapis-vmhost` image from
`test/e2e/Containerfile` first), then `test/e2e/run.sh phase-gpu`.

Third-party apps: copy `.apk`/`.xapk` files into `~/apps` in the VM, then
`test/e2e/run.sh phase-apps` (installs libhoudini unless `E2E_TRANSLATION` says
otherwise, and removes it again afterwards). The suite downloads nothing.

Phase 3 suspends the guest, so `all.sh` restarts the VM after it. Phase 4
replaces the dev install with packages and runs last.

Shared server (Debian/Ubuntu with Docker): copy `test/e2e/` plus `dist/` to
the server (`dist/` inside `test/e2e/`) and run
`E2E_COMPOSITOR=docker test/e2e/remote.sh`. It runs a
headless sway in Docker. On a machine other people use, never suspend, add
users or change packages other than lapis. Update the version string checked
in `remote.sh` when you release.

Helpers live in `test/e2e/lib.sh` (`step`, `check`, `wait_for`,
`window_size`, `app_follows_window`, ...); new checks go in the matching
`phaseN.sh`.

## Conventions

- **Naming:** the project is *lapis*. Protocol names that the Android images
  require stay in `internal/compat` and nowhere else. The upstream project
  that inspired lapis is mentioned only in the README credit sections.
- The only non-Go code is `internal/hwchook/csrc/hook.c`, which runs inside Android;
  it is embedded as prebuilt `.so` files, so the Go build stays cgo-free.
- Keep the binary static (`CGO_ENABLED=0`) and dependency-light; prefer the
  standard library and `golang.org/x/sys`. Do not shell out to tools the host
  may lack (iptables, dnsmasq, lxc, python).
- Match the surrounding code: short package doc comments, comments that
  explain *why*, gofmt formatting, errors wrapped with context.
- Privileged work belongs in `control` (root, socket-activated); user-facing
  work in `session`. Authenticate callers via SO_PEERCRED; never trust
  client-supplied UIDs.
- A frozen container must not hang the CLI: queries fail fast, launch/shell/stop
  thaw first.
- New user-visible behavior gets a README entry (English **and**
  [README-ID.md](README-ID.md)) and, where it shows on screen, a screenshot
  in `docs/screenshots/`.

## Commits and releases

- Author: `Eka Tresna Irawan <anak10thn@gmail.com>`. License: MIT.
- Plain, imperative commit subjects. No attribution or co-author trailers.
- CI (`.github/workflows/ci.yml`) runs gofmt, vet, tests and a package build
  on every push and PR to `main`.
- Release: bump the version where it is referenced (e.g. `remote.sh`), then

  ```sh
  git tag v0.6.0 && git push origin v0.6.0
  ```

  `.github/workflows/release.yml` builds every package for amd64 and arm64
  and publishes a GitHub release. Tags with a `-` (e.g. `v0.6.0-rc1`) become
  pre-releases.
