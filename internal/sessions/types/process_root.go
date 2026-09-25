package types

// ProcessRoot pins one local process a provider spawned: its pid plus the
// kernel start time, so a recycled pid never matches.
type ProcessRoot struct {
	PID       int
	StartSec  int64
	StartUsec int32
}

// RootReporter is implemented by providers that spawn a local process whose
// descendants may call back into the session host.
type RootReporter interface {
	// ProcessRoot reports the live spawned process. ok is false when there
	// is no local process, it has exited, or its start time is unreadable.
	ProcessRoot() (root ProcessRoot, ok bool)
}
