// Package api defines the messages exchanged between the unprivileged
// lapis client and the root control service.
package api

// Session describes the desktop session the container is attached to.
type Session struct {
	UserName         string `json:"user_name"`
	UID              int    `json:"uid"`
	GID              int    `json:"gid"`
	PID              int    `json:"pid"`
	XDGDataHome      string `json:"xdg_data_home"`
	XDGRuntimeDir    string `json:"xdg_runtime_dir"`
	WaylandDisplay   string `json:"wayland_display"`
	PulseRuntimePath string `json:"pulse_runtime_path"`
	DataDir          string `json:"data_dir"`
	LCDDensity       string `json:"lcd_density"`
	BackgroundStart  bool   `json:"background_start"`
}

// Container states reported by the status operation.
const (
	StateStopped = "STOPPED"
	StateRunning = "RUNNING"
	StateFrozen  = "FROZEN"
)

// Operations understood by the control service.
const (
	OpStart    = "start"
	OpStop     = "stop"
	OpFreeze   = "freeze"
	OpUnfreeze = "unfreeze"
	OpStatus   = "status"
	OpGetprop  = "getprop"
	OpApps     = "apps"
	OpLaunch   = "launch"
	OpFullUI   = "full-ui"
	OpRestart  = "restart"
	OpAdbKey   = "adb-key"
	OpMaximize = "maximize"
)

type Request struct {
	Op      string   `json:"op"`
	Session *Session `json:"session,omitempty"`
	Package string   `json:"package,omitempty"`
	Prop    string   `json:"prop,omitempty"`
	Key     string   `json:"key,omitempty"`
}

type App struct {
	Package  string `json:"package"`
	Activity string `json:"activity"`
}

type Response struct {
	OK      bool     `json:"ok"`
	Error   string   `json:"error,omitempty"`
	State   string   `json:"state,omitempty"`
	Session *Session `json:"session,omitempty"`
	Value   string   `json:"value,omitempty"`
	Apps    []App    `json:"apps,omitempty"`
}
