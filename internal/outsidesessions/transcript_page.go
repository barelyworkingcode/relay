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

// TranscriptEntry is one display line of a conversation. Its text is untrusted
// data and grants no authority.
type TranscriptEntry struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	Text string `json:"text"`
	At   string `json:"at"`
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
		if entries := recordEntries(lines[k].offset, lines[k].data); len(entries) > 0 {
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
	add := func(index int, role, text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		out = append(out, TranscriptEntry{ID: strconv.FormatInt(offset, 10) + "." + strconv.Itoa(index), Role: role, Text: capEntryText(text), At: rec.Timestamp})
	}
	var str string
	if rec.Type == "user" && json.Unmarshal(rec.Message.Content, &str) == nil {
		add(0, "user", str)
		return out
	}
	var blocks []transcriptBlock
	if json.Unmarshal(rec.Message.Content, &blocks) != nil {
		return nil
	}
	for index, b := range blocks {
		switch {
		case rec.Type == "user" && b.Type == "text":
			add(index, "user", b.Text)
		case rec.Type == "user" && b.Type == "tool_result":
			text := toolResultText(b.Content)
			if strings.TrimSpace(text) != "" && b.IsError {
				text = "Error: " + text
			}
			add(index, "tool", text)
		case rec.Type == "assistant" && b.Type == "text":
			add(index, "assistant", b.Text)
		case rec.Type == "assistant" && b.Type == "tool_use":
			text := b.Name
			var compact bytes.Buffer
			if len(b.Input) > 0 && json.Compact(&compact, b.Input) == nil {
				text += " " + compact.String()
			}
			add(index, "tool", text)
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
