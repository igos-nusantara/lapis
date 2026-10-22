# PLAN: lapis — Android runtime on systemd-nspawn

**Goal:** run Android with **Google Play and Google services** on any systemd-based
Linux desktop, sharing the **host kernel**, managed entirely by **systemd-nspawn +
systemd units** and a **single static Go binary**: no LXC, no Python, no
libgbinder, no dnsmasq, no iptables scripts.

lapis boots LineageOS 20 (Android 13) container images (`GAPPS` or `VANILLA`)
from an OTA channel and implements the whole host side itself. The project is
inspired by Waydroid (see README).

> An earlier revision of this plan proposed a KVM VM. It was dropped in favor of
> the container approach: lower RAM, native GPU and native per-app windows. The
> trade-off is that the host kernel must provide binder (§6).

**Status (2026-10-21): all five phases implemented and passing end-to-end tests**
in a VM and on a real Ubuntu server (§3).

---

## 1. Architecture

```
 Host (any distro, systemd >= 250, kernel with binder + binderfs)
 │
 ├─ lapis-control.socket  (/run/lapis/control.sock, Accept=yes, rate limited)
 │    └─ lapis-control@.service → `lapis control handle` (root, sandboxed, one per request)
 │         SO_PEERCRED auth; start/stop/freeze/restart/status/apps/launch/getprop/adb-key
 │
 ├─ lapis-android.service  (Type=notify, Delegate=yes, DevicePolicy=closed)
 │    ExecStartPre  `lapis container prepare`  binderfs nodes, device perms, mount
 │                                                system.img+vendor.img (+overlays), properties
 │    ExecStart     `lapis container exec`     seccomp filter → exec systemd-nspawn
 │                    PID 1 = /lapis-init (lapis shim) → exec /init
 │    ExecStopPost  `lapis container cleanup`
 │
 ├─ lapis-net.service  (sandboxed, PartOf the container unit)
 │    nftables NAT in own tables, DHCP server, DNS forwarder on lapis0,
 │    DOCKER-USER rules when Docker's firewall is present
 │
 └─ user: lapis-session.service → `lapis session start`
      ├─ pure-Go binder: publishes the user-monitor, clipboard, hardware and
      │  notification services into Android's servicemanager; calls the platform
      │  service; republishes after Android restarts
      ├─ .desktop entry per app (real labels, icons, "App Settings" action)
      ├─ logind delay inhibitor: freeze on host sleep, thaw on resume
      └─ notifications → org.freedesktop.Notifications, clipboard ↔ wl-clipboard

 Container (systemd-nspawn, host kernel)
   / = system.img (+overlay)   /vendor = vendor.img (+overlay)   /data = ~/.local/share/lapis/data
   /dev/{binder,vndbinder,hwbinder} ← host binderfs   /dev/dri/renderD* ← host GPU
   /run/xdg/wayland-0 ← host Wayland socket   /run/xdg/pulse/native ← host PipeWire/Pulse
   eth0 192.168.240.112 ── veth ── lapis0 192.168.240.1 (host, NAT)
```

The names the images look up (property namespace, binder service and interface
names, property file, window app ids, OTA channel) live in one place:
`internal/compat`.

---

## 2. Code layout (Go, `CGO_ENABLED=0`, static binary ~9 MB, amd64 + arm64)

```
cmd/lapis           CLI
internal/compat        Android-side names the images expect
internal/api           control protocol and session types
internal/binder        binder ioctl, parcels, service manager (AIDL), looper/client threads
internal/hostsvc       platform client; user-monitor/clipboard/hardware/notification services
internal/session       user session, .desktop sync, sleep handling, binder-first app launch
internal/control       privileged socket handler (input validation, ownership checks)
internal/container     nspawn args, PID-1 shim, prepare/exec/cleanup, attach (cgroup+seccomp)
internal/seccomp       BPF filter, verified in-kernel by a test
internal/netsvc        nftables, DHCP, DNS, Docker firewall integration
internal/wlproxy       Wayland proxy that fits app windows to their apps
internal/rootfs        loop mounts + overlays
internal/images        OTA channels, resumable download with mirror failover, sha256, lock
internal/extras        ARM translation installer (overlay + properties)
internal/props, gpu, drivers, config, paths, sysutil, units, doctor
packaging/, scripts/release.sh     nfpm config, release build
test/e2e/              VM harness, per-phase suites, shared-server suite
```

---

## 3. Verification

### Unit tests (`go test ./...`)
Binder parcels and exception headers, seccomp program (BPF interpreter **and** a
real in-kernel load), nspawn arguments, properties, OTA layout, resumable download
and slow-mirror failover, desktop labels, systemd unit syntax.

### End-to-end in a VM
`test/e2e/vm.sh` boots an Arch Linux VM (KVM inside a rootless podman container,
so the development host stays untouched). The stock Arch kernel ships binder (the
Rust binder driver). A headless **sway** session stands in for the desktop.
`test/e2e/all.sh` runs every phase; each phase was green before the next started.

| Phase | Suite | Checks | What it proves |
|---|---|---|---|
| 1 Bring-up | `phase1.sh` | 27 | GAPPS image boots under nspawn; binder, network+DNS, Wayland hwcomposer, per-app host window, Play Store/GMS, freeze/thaw, GSF id, clean stop and restart |
| 2 Binder services | `phase2.sh` | 30 | Services visible in Android's servicemanager; platform calls; unlock and install/uninstall events update .desktop files; launch/intent over binder without root; clipboard both ways; notifications reach the desktop; suspend/reboot requests; republish after Android reboot |
| 3 Integration | `phase3.sh` | 32 | Full-UI window; adb with auto-authorized key; other users cannot control the container; host suspend via logind freezes/thaws; ARM-only APK refused without and runs with libndk |
| 4 Packaging | `phase4.sh` | 26 | Checksums; deb/rpm contents; Arch package and systemd-sysext each install, boot Android, open a window, uninstall cleanly |
| 5 Hardening | `phase5.sh` | 26 | Unit sandboxing; container under seccomp, without SYS_MODULE/SYS_RAWIO; device policy blocks host disks; attached commands confined; control service rejects uid spoofing, foreign paths, injection, garbage |

### GPU (`phase-gpu.sh`, VM started with `E2E_GPU=virgl`)
The VM gets a virtio-gpu whose virgl renderer runs on the development host's GPU
(Steam Deck, AMD Van Gogh / radeonsi), and sway uses its GLES renderer. Android
picks the render node, minigbm and Mesa: SurfaceFlinger reports
`GLES: Mesa, virgl (AMD Custom GPU 0405 (radeonsi, vangogh, …)), OpenGL ES 3.2`.
11 checks pass: hardware GLES, a GL game (Mindustry) installs, opens its own host
window and renders at ≥ 30 fps (61 fps measured, vsync-bound). SuperTuxKart (3D,
GLES 3) races at 61 fps in the full-UI window
(`docs/screenshots/gpu-supertuxkart.png`, `gpu-mindustry.png`).

Finding: in per-app windows the image's hwcomposer only gives a window to layers
named `TID:<task>#…`. Some apps (SuperTuxKart's SDL SurfaceView) get a layer name
without that prefix, so the layer is dropped and the window shows only its
decoration. Those apps work in the full-UI window. Fixing it needs a change to the
image's hwcomposer or framework, not to lapis.

### End-to-end on a shared server
`test/e2e/remote.sh` (run with `E2E_COMPOSITOR=docker`) covers the phases that are
safe on a machine other people use (no suspend, no new users), installing from the
`.deb` on Ubuntu 24.04 (kernel 6.8, binder as a module, NVIDIA GPU → software
rendering, Docker present). See the results in §3.1.

#### 3.1 Server results (Ubuntu 24.04, 32 cores, Tesla T4, Docker, kernel 6.8)
**All checks pass** with the `.deb` (65 checks in the latest suite): install via apt (pulls
`systemd-container`), image download with mirror failover, boot with software
rendering, internet through Docker's firewall, every phase-2/3/5 check that is safe
on a shared machine (binder services, app windows, freeze, libndk + ARM-only app,
seccomp/capabilities/device policy, input validation), Android-initiated reboot and
a clean stop that leaves no mounts, links, nftables or DOCKER-USER rules behind.

Screenshots from that server (headless sway/weston in Docker):
`docs/screenshots/full-ui.png` (whole Android desktop in one window) and
`docs/screenshots/app-windows.png` (Settings and Browser as two app-sized host
windows, phone mode) and `docs/screenshots/tablet-settings.png` (Settings in
tablet mode).

### Findings from the e2e runs (handled in code)
- Android chowns `/data` to its system user (uid 1000); the control service now
  validates the data directory's parent, which only showed up on a host whose
  user is not uid 1000.
- Modules (`binder_linux`, `loop`) must be loaded before the container unit starts,
  because `DeviceAllow=` classes are resolved at start: `lapis-binder.service`
  does that.
- systemd-nspawn 255 does not accept the PID-1 shim at a dot-file path.
- Package events come from the image's launcher and are missed while it is not
  running; the session also compares the app list every 15 s.
- Release tarballs now store files as root:root (they were owned by the build user).
- Android asks to suspend after its screen timeout and gets frozen (as the
  original runtime does). Commands into a frozen container hang, so queries now
  fail fast, launch/shell thaw first (shell re-freezes), stopping thaws first,
  reattaching a session thaws, and the periodic app sync leaves Android asleep.
- Right after boot Android runs user unlock twice and reinstalls system apps in
  between; right after a package change PackageManager briefly returns partial
  lists. Desktop entries are pruned only after two consistent checks and never
  from a list that shrank by half.
- Android remembers each app's last window bounds; switching display mode clears
  them so apps open the way the new mode wants.
- `lapis session start` refuses a Wayland socket nobody listens on.
- App windows were as large as the Android display (the images' hwcomposer
  sets the display as window geometry), and any compositor size suggestion
  resized the whole Android display (a floating WM's 75x50 minimum, or a tile).
  A Wayland proxy in the session (`internal/wlproxy`) now sets each app
  window's geometry to its layers' extent and neutralizes size suggestions;
  e2e checks the host window size in phone and tablet mode.
- The other direction: a size the compositor asks for an app window (a user
  resize, a tile, maximize) becomes an Android task resize, debounced, with
  placeholder sizes (a floating WM's 75x50) ignored; the launch-time maximize
  steps aside when the host window already chose a size. e2e resizes a floating
  window and tiles two apps side by side.
- systemd-nspawn's `/sys` has no `/sys/fs/bpf`, so Android's bpfloader could not
  pin its network maps and `system_server` crashed on every app removal
  (`NetworkStatsService` NPE on `UID_REMOVED`). lapis now mounts a private bpf
  filesystem instance for the container; app install/uninstall is an e2e check.
- Binder calls to a frozen Android block; the session's periodic work runs
  beside its main loop so stopping a session never waits on them.
- The images' default density gave a ~384dp phone layout on desktop displays and
  apps opened in phone-sized freeform windows. `[display] mode = "tablet"` (the
  default) sets 160 dpi (sw ≥ 600dp) and maximizes each launched app's task, which
  gives tablet layouts; `phone` restores the old behavior.
- The images' first-stage init creates `/dev/random`/`urandom` itself; nspawn
  pre-creates them → PID-1 shim removes them before exec'ing `/init`.
- The hwcomposer only maps windows of packages listed in the active-apps
  property; intents are re-sent once the handling package is allowed, and the
  full UI uses a status-bar refresh.
- Binder death notifications cannot target handle 0; the session watches the
  servicemanager's own `manager` service instead.
- Device class names differ between C and Rust binder (`binder` vs `rust_binder`);
  both are allowed in the device policy.
- Docker sets the `FORWARD` policy to DROP; lapis adds and removes its own
  `DOCKER-USER` rules.
- Download mirrors differ by 100×; slow, stalled or dropped mirrors are abandoned
  and the download resumes elsewhere. Concurrent `init`/`upgrade` runs are locked out.

---

## 4. Remaining work

- **SteamOS:** the kernel has no binder (§6). lapis itself installs cleanly as a
  systemd-sysext.
- Bare-metal runs with an AMD/Intel GPU (GPU tests so far: virgl on an AMD host).
- Games whose SurfaceView layer lacks the task prefix stay black in per-app windows
  (§3, GPU); use the full-UI window for them.
- Notifications posted by the Android shell are not forwarded by Android itself
  (app notifications are).
- Binder devices are world-accessible, so another local user could call the
  platform service directly; the control service and container lifecycle are
  protected.
- CI: run `test/e2e/all.sh` on a self-hosted runner with KVM.

---

## 5. Google Play & Google services

- `sudo lapis init -type GAPPS` installs LineageOS + GApps.
- Certification: `sudo lapis gapps-id` → register at
  `https://www.google.com/android/uncertified` → clear Play Store data / restart.
- Play Integrity: at most BASIC; DEVICE/STRONG apps (banking, Wallet, some DRM)
  will not work in any container or emulator.
- ARM-only apps: `sudo lapis extras install libndk` (AMD) or `libhoudini` (Intel).

---

## 6. Kernel requirement: binder

| Distro kernel | binder |
|---|---|
| Arch `linux` / `linux-zen` | built in (Rust binder on recent kernels) |
| Ubuntu generic | module `binder_linux` (loaded automatically) |
| Fedora / Debian | varies; `lapis doctor` reports it |
| **SteamOS** | **absent** |

SteamOS options, from least to most intrusive:
1. Build `binder_linux` against the matching `linux-neptune` headers, keep the
   `.ko` in `/var/lib/lapis/modules`, load it from a unit; rebuild after every
   SteamOS kernel update.
2. Custom kernel — not recommended.
3. The KVM design from the first plan revision.

---

## 7. Resource expectations

- RAM: ~1–1.5 GB idle with GApps; the VM test peaked at 2.6 GB for the container.
- GPU: native Mesa on the host render node (software rendering without one).
- `freeze` stops CPU use instantly while keeping RAM; done automatically on sleep.
