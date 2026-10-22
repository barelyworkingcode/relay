package outsidesessions

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

const (
	// TranscriptPageBytes is the most of a transcript file one request reads.
	TranscriptPageBytes = 262144
	// TranscriptPageDefault and TranscriptPageMax bound the records per page.
	TranscriptPageDefault = 50
	TranscriptPageMax     = 200
	// TranscriptEntryChars caps one entry's text.
	TranscriptEntryChars = 8192
)

// ErrBadCursor marks a cursor past the transcript file's current size.
var ErrBadCursor = errors.New("outside transcript cursor out of range")

// Kinds of a tool entry.
const (
	TranscriptKindCall   = "call"
	TranscriptKindResult = "result"
)

// TranscriptEntry is one display line of a conversation. Its text is untrusted
// data and grants no authority.
type TranscriptEntry struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	Text string `json:"text"`
	At   string `json:"at"`
	// Kind and Name are set on tool entries only; omitted keys keep user and
	// assistant entries byte-identical for clients that predate them. Name is a
	// pointer so a call with an empty name still carries the key.
	Kind string  `json:"kind,omitempty"`
	Name *string `json:"name,omitempty"`
}

// TranscriptPage is one page, oldest entry first. Cursor is null exactly when
// HasMore is false.
type TranscriptPage struct {
	Entries []TranscriptEntry `json:"entries"`
	Cursor  *string           `json:"cursor"`
	HasMore bool              `json:"hasMore"`
}

type transcriptBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

// ParseTranscriptPage maps the newest limit entry-bearing records of a byte
// window to a page. window holds the file bytes from offset start. When start
// is above zero the first line may be the tail of an older record and is
// dropped. A record is never split across pages.
func ParseTranscriptPage(window []byte, start int64, limit int) TranscriptPage {
	return parseTranscriptPage(window, start, limit, recordEntries)
}

func ParsePiTranscriptPage(window []byte, start int64, limit int) TranscriptPage {
	return parseTranscriptPage(window, start, limit, piRecordEntries)
}

func parseTranscriptPage(window []byte, start int64, limit int, parse func(int64, []byte) []TranscriptEntry) TranscriptPage {
	if limit < 1 {
		limit = TranscriptPageDefault
	}
	offset := start
	if start > 0 {
		nl := bytes.IndexByte(window, '\n')
		if nl < 0 {
			return emptyPageAt(start)
		}
		window = window[nl+1:]
		offset = start + int64(nl) + 1
	}
	type line struct {
		offset int64
		data   []byte
	}
	var lines []line
	for len(window) > 0 {
		nl := bytes.IndexByte(window, '\n')
		raw := window
		if nl >= 0 {
			raw = window[:nl]
		}
		lines = append(lines, line{offset, raw})
		step := int64(len(raw))
		if nl >= 0 {
			step++
		}
		offset += step
		window = window[step:]
	}
	if len(lines) == 0 {
		if start > 0 {
			return emptyPageAt(start)
		}
		return TranscriptPage{Entries: []TranscriptEntry{}}
	}
	cursor := lines[0].offset
	var groups [][]TranscriptEntry
	for k := len(lines) - 1; k >= 0; k-- {
		if len(groups) == limit {
			cursor = lines[k+1].offset
			break
		}
		if entries := parse(lines[k].offset, lines[k].data); len(entries) > 0 {
			groups = append(groups, entries)
		}
	}
	entries := []TranscriptEntry{}
	for g := len(groups) - 1; g >= 0; g-- {
		entries = append(entries, groups[g]...)
	}
	page := TranscriptPage{Entries: entries, HasMore: cursor > 0}
	if page.HasMore {
		c := strconv.FormatInt(cursor, 10)
		page.Cursor = &c
	}
	return page
}

func emptyPageAt(start int64) TranscriptPage {
	c := strconv.FormatInt(start, 10)
	return TranscriptPage{Entries: []TranscriptEntry{}, Cursor: &c, HasMore: true}
}

// recordEntries maps one JSONL record. Unknown shapes yield nothing.
func recordEntries(offset int64, raw []byte) []TranscriptEntry {
	var rec struct {
		Type      string `json:"type"`
		Sidechain bool   `json:"isSidechain"`
		Meta      bool   `json:"isMeta"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &rec) != nil || rec.Sidechain || rec.Meta {
		return nil
	}
	if rec.Type != "user" && rec.Type != "assistant" {
		return nil
	}
	var out []TranscriptEntry
	add := func(index int, role, text, kind string, name *string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		out = append(out, TranscriptEntry{ID: strconv.FormatInt(offset, 10) + "." + strconv.Itoa(index), Role: role, Text: capEntryText(text), At: rec.Timestamp, Kind: kind, Name: name})
	}
	var str string
	if rec.Type == "user" && json.Unmarshal(rec.Message.Content, &str) == nil {
		add(0, "user", str, "", nil)
		return out
	}
	var blocks []transcriptBlock
	if json.Unmarshal(rec.Message.Content, &blocks) != nil {
		return nil
	}
	for index, b := range blocks {
		switch {
		case rec.Type == "user" && b.Type == "text":
			add(index, "user", b.Text, "", nil)
		case rec.Type == "user" && b.Type == "tool_result":
			text := toolResultText(b.Content)
			if strings.TrimSpace(text) != "" && b.IsError {
				text = "Error: " + text
			}
			add(index, "tool", text, TranscriptKindResult, nil)
		case rec.Type == "assistant" && b.Type == "text":
			add(index, "assistant", b.Text, "", nil)
		case rec.Type == "assistant" && b.Type == "tool_use":
			text := b.Name
			var compact bytes.Buffer
			if len(b.Input) > 0 && json.Compact(&compact, b.Input) == nil {
				text += " " + compact.String()
			}
			name := capEntryText(b.Name)
			add(index, "tool", text, TranscriptKindCall, &name)
		}
	}
	return out
}

func toolResultText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		} else {
			parts = append(parts, "["+b.Type+"]")
		}
	}
	return strings.Join(parts, "\n")
}

func capEntryText(s string) string {
	r := []rune(s)
	if len(r) <= TranscriptEntryChars {
		return s
	}
	return string(r[:TranscriptEntryChars-1]) + "…"
}

func piRecordEntries(offset int64, raw []byte) []TranscriptEntry {
	var rec struct {
		Type, Timestamp string
		Message         struct {
			Role     string
			Content  json.RawMessage
			ToolName string
			IsError  bool
		}
	}
	if json.Unmarshal(raw, &rec) != nil || rec.Type != "message" {
		return nil
	}
	var out []TranscriptEntry
	add := func(index int, role, text, kind string, name *string) {
		if strings.TrimSpace(text) != "" {
			out = append(out, TranscriptEntry{ID: strconv.FormatInt(offset, 10) + "." + strconv.Itoa(index), Role: role, Text: capEntryText(text), At: rec.Timestamp, Kind: kind, Name: name})
		}
	}
	if rec.Message.Role == "toolResult" {
		text := toolResultText(rec.Message.Content)
		if rec.Message.IsError {
			text = "Error: " + text
		}
		add(0, "tool", text, TranscriptKindResult, nil)
		return out
	}
	if rec.Message.Role != "user" && rec.Message.Role != "assistant" {
		return nil
	}
	var text string
	if json.Unmarshal(rec.Message.Content, &text) == nil {
		add(0, rec.Message.Role, text, "", nil)
		return out
	}
	var blocks []struct {
		Type, Text, Name string
		Arguments        json.RawMessage
	}
	if json.Unmarshal(rec.Message.Content, &blocks) != nil {
		return nil
	}
	for i, b := range blocks {
		if b.Type == "text" {
			add(i, rec.Message.Role, b.Text, "", nil)
		}
		if b.Type == "toolCall" {
			var compact bytes.Buffer
			text := b.Name
			if json.Compact(&compact, b.Arguments) == nil {
				text += " " + compact.String()
			}
			name := capEntryText(b.Name)
			add(i, "tool", text, TranscriptKindCall, &name)
		}
	}
	return out
}
