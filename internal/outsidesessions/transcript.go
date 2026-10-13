package outsidesessions

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

const TranscriptTailBytes = 1 << 20

func tailCharacters(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[len(r)-n:])
	}
	return s
}

// LatestTurn drops older answers at each genuine user prompt. Tool-result user
// messages belong to the same turn. A truncated tail without a user boundary
// cannot prove its latest answer and therefore returns no cached text.
func LatestTurn(data []byte) Turn {
	var t Turn
	found := false
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var v struct {
			Type      string `json:"type"`
			Sidechain bool   `json:"isSidechain"`
			Timestamp string `json:"timestamp"`
			Subtype   string `json:"subtype"`
			Message   struct {
				Role       string          `json:"role"`
				Content    json.RawMessage `json:"content"`
				StopReason *string         `json:"stop_reason"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &v) != nil || v.Sidechain {
			continue
		}
		if v.Type == "user" {
			var blocks []struct {
				Type string `json:"type"`
			}
			isTool := false
			if json.Unmarshal(v.Message.Content, &blocks) == nil {
				for _, b := range blocks {
					if b.Type == "tool_result" {
						isTool = true
					}
				}
			}
			if !isTool {
				t = Turn{}
				found = true
			}
			continue
		}
		if !found {
			continue
		}
		if v.Type == "assistant" {
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			var text []string
			if json.Unmarshal(v.Message.Content, &blocks) == nil {
				for _, b := range blocks {
					if b.Type == "text" {
						text = append(text, b.Text)
					}
				}
			} else {
				var s string
				if json.Unmarshal(v.Message.Content, &s) == nil {
					text = append(text, s)
				}
			}
			if len(text) > 0 {
				if t.Text != "" {
					t.Text += "\n"
				}
				t.Text += strings.Join(text, "\n")
				t.Text = tailCharacters(t.Text, 8192)
				t.At = v.Timestamp
			}
			t.Complete = v.Message.StopReason != nil && *v.Message.StopReason == "end_turn"
		}
		if v.Type == "system" && (v.Subtype == "turn_duration" || v.Subtype == "stop") {
			t.Complete = true
		}
	}
	return t
}

// ProbeState estimates a paused tool request as asking. It is display data,
// not a permission decision; only hooks identify permission prompts exactly.
func ProbeState(data []byte) (string, time.Time) {
	state := "idle"
	var at time.Time
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var row struct {
			Type      string `json:"type"`
			Sidechain bool   `json:"isSidechain"`
			Timestamp string `json:"timestamp"`
			Message   struct {
				Content    json.RawMessage `json:"content"`
				StopReason string          `json:"stop_reason"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &row) != nil || row.Sidechain {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, row.Timestamp)
		if err != nil || t.Before(at) {
			continue
		}
		switch row.Type {
		case "user":
			state = "running"
		case "assistant":
			state = "running"
			if row.Message.StopReason == "end_turn" {
				state = "idle"
			}
			var blocks []struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(row.Message.Content, &blocks) == nil {
				for _, b := range blocks {
					if b.Type == "tool_use" {
						state = "asking"
					}
				}
			}
		default:
			continue
		}
		at = t
	}
	return state, at
}
