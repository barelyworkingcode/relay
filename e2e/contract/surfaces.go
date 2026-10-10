package contract

// Surface names one group of behaviour that fakerelay serves and relay must
// serve the same way. Every Surface has at least one Scenario
// (TestSurfacesCovered).
type Surface string

// The surfaces. WSHub is the /ws surface; it cannot be named WS, which is the
// WebSocket type. Fake-only names (the control socket, ctl, faults, fakerelay.*
// events) belong to none of them.
const (
	Bridge       Surface = "bridge"
	Dispatch     Surface = "dispatch"
	Projects     Surface = "projects"
	ChiefOfStaff Surface = "chief-of-staff"
	Hosts        Surface = "hosts"
	Files        Surface = "files"
	WSFiles      Surface = "ws-files"
	Sessions     Surface = "sessions"
	WSHub        Surface = "ws"
	Terminals    Surface = "terminals"
	MCPs         Surface = "mcps"
	Eve          Surface = "eve"
	Audit        Surface = "audit"
	Events       Surface = "events"
	CLI          Surface = "cli"
	Doors        Surface = "doors"
	Presence     Surface = "presence"
)

// Surfaces lists every Surface constant.
var Surfaces = []Surface{
	Bridge, Dispatch, Projects, ChiefOfStaff, Hosts, Files, WSFiles, Sessions, WSHub,
	Terminals, MCPs, Eve, Audit, Events, CLI, Doors, Presence,
}
