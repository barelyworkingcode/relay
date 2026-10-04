package logging

import (
	"log/slog"
	"strings"
)

// ParseLevel maps the four names the logging standard allows. Anything else,
// including the empty string, reports false so the caller can tell "unset"
// from "invalid" by checking the input itself.
func ParseLevel(s string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error":
		return slog.LevelError, true
	case "warn":
		return slog.LevelWarn, true
	case "info":
		return slog.LevelInfo, true
	case "debug":
		return slog.LevelDebug, true
	}
	return slog.LevelInfo, false
}

// levelName rounds a level between the four names down to the nearest one
// below it; anything at or above error is error.
func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	}
	return "debug"
}
