package config

import "time"

// OutsideSessionsConfig tunes the outside-session list. nil fields mean the
// default.
type OutsideSessionsConfig struct {
	// EndedWindowDays is how many days an ended outside session stays listed.
	// nil: the default. 0: ended rows are off.
	EndedWindowDays *int `json:"ended_window_days,omitempty"`
}

const (
	OutsideEndedWindowDefaultDays = 3
	OutsideEndedWindowMaxDays     = 30
)

// OutsideEndedWindow is how long an ended outside session stays listed. An
// absent setting is the default; a value outside 0..OutsideEndedWindowMaxDays
// fails closed to 0, which lists no ended rows.
func (s *Settings) OutsideEndedWindow() time.Duration {
	if s == nil || s.OutsideSessions == nil || s.OutsideSessions.EndedWindowDays == nil {
		return OutsideEndedWindowDefaultDays * 24 * time.Hour
	}
	days := *s.OutsideSessions.EndedWindowDays
	if days < 0 || days > OutsideEndedWindowMaxDays {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}
