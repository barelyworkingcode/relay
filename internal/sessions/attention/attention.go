// Package attention tracks what each agent session is doing: one of seven
// states, the turn's reply excerpt, and a stall sweep. The transition table is
// Next; Board applies it per session and tells a Sink.
package attention

import "time"

type State string

const (
	Starting State = "starting"
	Running  State = "running"
	Idle     State = "idle"
	Asking   State = "asking"
	Errored  State = "errored"
	Stalled  State = "stalled"
	Ended    State = "ended"
)

const (
	StallAfter    = 300 * time.Second
	SweepInterval = 5 * time.Second
	ExcerptRunes  = 500
)

// SessionEnded is the contract's Ended signal; the State constant Ended
// already owns that name in this package.
type Signal int

const (
	Launching Signal = iota + 1
	Launched
	LaunchFailed
	TurnStarted
	Activity
	Asked
	Answered
	TurnEnded
	TurnFailed
	TurnStopped
	ProcessExited
	StallTimeout
	SessionEnded
)

// Next is the whole state machine. cur "" means no entry. A pair with no row
// returns cur unchanged.
func Next(cur State, sig Signal) State {
	switch sig {
	case Launching:
		return Starting
	case SessionEnded:
		return Ended
	}
	switch cur {
	case Starting:
		switch sig {
		case Launched:
			return Idle
		case LaunchFailed:
			return Errored
		case TurnStarted:
			return Running
		case ProcessExited:
			return Ended
		}
	case Idle:
		switch sig {
		case TurnStarted:
			return Running
		case ProcessExited:
			return Ended
		}
	case Running:
		switch sig {
		case Asked:
			return Asking
		case StallTimeout:
			return Stalled
		case TurnEnded, TurnStopped:
			return Idle
		case TurnFailed, ProcessExited:
			return Errored
		}
	case Asking:
		switch sig {
		case Answered:
			return Running
		case TurnEnded, TurnStopped:
			return Idle
		case TurnFailed, ProcessExited:
			return Errored
		}
	case Stalled:
		switch sig {
		case Activity:
			return Running
		case Asked:
			return Asking
		case TurnEnded, TurnStopped:
			return Idle
		case TurnFailed, ProcessExited:
			return Errored
		}
	case Errored:
		if sig == TurnStarted {
			return Running
		}
	}
	return cur
}
