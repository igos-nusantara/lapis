// Package paths holds every well-known location used by lapis on the
// host and inside the container.
package paths

const (
	WorkDir     = "/var/lib/lapis"
	ConfigFile  = WorkDir + "/lapis.toml"
	ImagesDir   = WorkDir + "/images"
	RootfsDir   = WorkDir + "/rootfs"
	OverlayDir  = WorkDir + "/overlay"
	OverlayRW   = WorkDir + "/overlay_rw"
	OverlayWork = WorkDir + "/overlay_work"
	HostPerms   = WorkDir + "/host-permissions"

	RunDir        = "/run/lapis"
	SessionFile   = RunDir + "/session.json"
	PropFile      = RunDir + "/android.prop"
	ControlSocket = RunDir + "/control.sock"
	BPFDir        = RunDir + "/bpf"

	Machine        = "lapis"
	ContainerUnit  = "lapis-android.service"
	NetworkUnit    = "lapis-net.service"
	BinderUnit     = "lapis-binder.service"
	ControlSocketU = "lapis-control.socket"

	// Paths inside the container, passed to Android through properties.
	ContainerXDGRuntimeDir  = "/run/xdg"
	ContainerWaylandDisplay = "wayland-0"
	ContainerPulseDir       = ContainerXDGRuntimeDir + "/pulse"
)
