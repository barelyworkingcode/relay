package config

import "time"

// SessionHistoryConfig tunes the session history. nil fields mean the default.
type SessionHistoryConfig struct {
	// PurgeAfterDays is how many days a history record whose transcript is
	// gone stays listed before relay removes it. nil: the default. 0: off.
	PurgeAfterDays *int `json:"purge_after_days,omitempty"`
}

const (
	SessionHistoryPurgeDefaultDays = 7
	SessionHistoryPurgeMaxDays     = 90
)

// PurgeGrace is how long a record stays after its transcript goes missing; 0
// means purge is off. A value outside 0..SessionHistoryPurgeMaxDays fails
// closed to 0: a typo never starts deleting sooner than asked.
func (s *Settings) PurgeGrace() time.Duration {
	if s == nil || s.SessionHistory == nil || s.SessionHistory.PurgeAfterDays == nil {
		return SessionHistoryPurgeDefaultDays * 24 * time.Hour
	}
	days := *s.SessionHistory.PurgeAfterDays
	if days < 0 || days > SessionHistoryPurgeMaxDays {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}

func cloneSessionHistoryConfig(c *SessionHistoryConfig) *SessionHistoryConfig {
	if c == nil {
		return nil
	}
	cp := *c
	if c.PurgeAfterDays != nil {
		d := *c.PurgeAfterDays
		cp.PurgeAfterDays = &d
	}
	return &cp
}
