package adopt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	recoveryPromptChars = 300
	recoveryInputChars  = 500
	recoveryNameChars   = 128
)

type tailRecord struct {
	Type      string `json:"type"`
	Sidechain bool   `json:"isSidechain"`
	Meta      bool   `json:"isMeta"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type tailBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
}

// parseTailRecords walks a transcript tail oldest first and skips a line that
// is not JSON, which includes a first line the window cut in half. Sub-agent
// (sidechain) records are another conversation's and are skipped.
func parseTailRecords(data []byte, visit func(rec tailRecord, blocks []tailBlock, text string)) {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var rec tailRecord
		if json.Unmarshal(line, &rec) != nil || rec.Sidechain {
			continue
		}
		if rec.Type != "user" && rec.Type != "assistant" {
			continue
		}
		var blocks []tailBlock
		var text string
		if json.Unmarshal(rec.Message.Content, &blocks) != nil {
			blocks = nil
			if json.Unmarshal(rec.Message.Content, &text) != nil {
				text = ""
			}
		}
		visit(rec, blocks, text)
	}
}

// ReadTail reads what a transcript tail says about the session. The text is
// untrusted: it is data for a message, never for a decision about access.
func ReadTail(data []byte) Tail {
	var t Tail
	open := map[string]*ToolCall{}
	var order []string
	parseTailRecords(data, func(rec tailRecord, blocks []tailBlock, text string) {
		if at, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil && at.After(t.LastRecord) {
			t.LastRecord = at
		}
		switch rec.Type {
		case "assistant":
			if family := modelFamily(rec.Message.Model); family != "" {
				t.ModelFamily = family
			}
			for _, b := range blocks {
				if b.Type == "tool_use" && b.ID != "" {
					open[b.ID] = &ToolCall{ID: b.ID, Name: b.Name, Input: b.Input}
					order = append(order, b.ID)
				}
			}
		case "user":
			genuine := !rec.Meta
			var texts []string
			for _, b := range blocks {
				switch b.Type {
				case "tool_result":
					delete(open, b.ToolUseID)
					genuine = false
				case "text":
					texts = append(texts, b.Text)
				}
			}
			if text != "" {
				texts = append(texts, text)
			}
			if genuine && len(texts) > 0 {
				t.LastPrompt = strings.Join(texts, "\n")
			}
		}
	})
	for i := len(order) - 1; i >= 0; i-- {
		if call, ok := open[order[i]]; ok {
			t.OpenTool = call
			break
		}
	}
	return t
}

// modelFamily maps a model id to the alias claude accepts for it. A synthetic
// or unknown id answers "".
func modelFamily(model string) string {
	for _, family := range []string{"haiku", "sonnet", "opus"} {
		if strings.HasPrefix(model, "claude-"+family+"-") {
			return family
		}
	}
	return ""
}

// firstUserAfter is the timestamp of the first user record written at or after
// start; the zero time when the tail holds none.
func firstUserAfter(data []byte, start time.Time) time.Time {
	var first time.Time
	parseTailRecords(data, func(rec tailRecord, _ []tailBlock, _ string) {
		if rec.Type != "user" || !first.IsZero() {
			return
		}
		if at, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil && !at.Before(start) {
			first = at
		}
	})
	return first
}

// RecoveryText is the one message relay sends into a session it took over in
// the middle of a turn. It quotes the session's own transcript, cut and on one
// line each, so that text cannot add lines to the message.
func RecoveryText(t Tail) string {
	var b strings.Builder
	b.WriteString("Relay took over this session from a terminal while a turn was running, and interrupted that turn.\n")
	fmt.Fprintf(&b, "You were working on: \"%s\"\n", oneLine(t.LastPrompt, recoveryPromptChars))
	if t.OpenTool == nil {
		b.WriteString("No tool call was open.\n")
		b.WriteString("Check the state of anything this turn touched before you continue.")
		return b.String()
	}
	input := "null"
	var compact bytes.Buffer
	if json.Compact(&compact, t.OpenTool.Input) == nil && compact.Len() > 0 {
		input = compact.String()
	}
	fmt.Fprintf(&b, "The open tool call was %s (id %s) with input %s.\n",
		oneLine(t.OpenTool.Name, recoveryNameChars), oneLine(t.OpenTool.ID, recoveryNameChars), oneLine(input, recoveryInputChars))
	b.WriteString("That call was cut off before its result was recorded. Check whether it took effect, and check the state of anything it touched, before you continue.")
	return b.String()
}

// oneLine joins s's words with single spaces and keeps at most max characters,
// the last being "…" when it cut.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max-1]) + "…"
}
