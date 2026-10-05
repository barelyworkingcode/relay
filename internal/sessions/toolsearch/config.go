package toolsearch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// Mode selects when tool search is active.
type Mode string

const (
	ModeAuto Mode = "auto"
	ModeOn   Mode = "on"
	ModeOff  Mode = "off"
)

const (
	maxConfigBytes   = 64 << 10
	maxLoadedCeiling = 200
)

// Config is the toolSearch object of chat.json.
type Config struct {
	Mode      Mode     `json:"mode"`
	Threshold float64  `json:"threshold"`
	MaxLoaded int      `json:"maxLoaded"`
	Pinned    []string `json:"pinned"`
}

// DefaultConfig is what a missing file or missing keys mean.
func DefaultConfig() Config {
	return Config{Mode: ModeAuto, Threshold: 0.1, MaxLoaded: 20}
}

// rawConfig uses pointers so an absent key keeps its default.
type rawConfig struct {
	Mode      *string   `json:"mode"`
	Threshold *float64  `json:"threshold"`
	MaxLoaded *int      `json:"maxLoaded"`
	Pinned    *[]string `json:"pinned"`
}

// LoadConfig reads path. A missing file yields DefaultConfig. Anything
// invalid yields Config{Mode: "off"} and an error naming the path and the
// field, so a typo turns the feature off instead of silently changing it.
func LoadConfig(path string) (Config, error) {
	off := Config{Mode: ModeOff}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return DefaultConfig(), nil
		}
		return off, fmt.Errorf("%s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return off, fmt.Errorf("%s: %w", path, err)
	}
	if len(data) > maxConfigBytes {
		return off, fmt.Errorf("%s: file larger than %d bytes", path, maxConfigBytes)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return off, fmt.Errorf("%s: invalid JSON: %w", path, err)
	}
	section, ok := top["toolSearch"]
	if !ok {
		return DefaultConfig(), nil
	}
	var raw rawConfig
	dec := json.NewDecoder(bytes.NewReader(section))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return off, fmt.Errorf("%s: toolSearch: %w", path, err)
	}

	cfg := DefaultConfig()
	if raw.Mode != nil {
		switch m := Mode(*raw.Mode); m {
		case ModeAuto, ModeOn, ModeOff:
			cfg.Mode = m
		default:
			return off, fmt.Errorf("%s: toolSearch.mode: %q is not auto, on or off", path, *raw.Mode)
		}
	}
	if raw.Threshold != nil {
		if t := *raw.Threshold; !(t > 0 && t <= 1) {
			return off, fmt.Errorf("%s: toolSearch.threshold: %v is not in (0, 1]", path, t)
		}
		cfg.Threshold = *raw.Threshold
	}
	if raw.MaxLoaded != nil {
		if n := *raw.MaxLoaded; n < 1 || n > maxLoadedCeiling {
			return off, fmt.Errorf("%s: toolSearch.maxLoaded: %d is not in 1..%d", path, n, maxLoadedCeiling)
		}
		cfg.MaxLoaded = *raw.MaxLoaded
	}
	if raw.Pinned != nil {
		for i, p := range *raw.Pinned {
			if p == "" {
				return off, fmt.Errorf("%s: toolSearch.pinned[%d]: empty name", path, i)
			}
		}
		cfg.Pinned = append([]string(nil), *raw.Pinned...)
	}
	return cfg, nil
}
