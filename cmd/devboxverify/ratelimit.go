package main

import (
	"encoding/json"
	"strings"
	"time"
)

// limitFrame is what one /ws frame says about the provider's usage limit.
// Only llm_event frames fill it.
type limitFrame struct {
	// Hit marks the assistant message_start of a turn the CLI ended with
	// error "rate_limit". The structured field decides; reply text never does.
	Hit bool
	// ResetsAt is a rejected rate_limit_event's resetsAt, in Unix seconds.
	ResetsAt int64
	// Text is an assistant text_delta's text.
	Text string
}

func parseLimitFrame(raw []byte) limitFrame {
	var f struct {
		Type  string `json:"type"`
		Event struct {
			Type  string `json:"type"`
			Error string `json:"error"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
			Info struct {
				Status   string `json:"status"`
				ResetsAt int64  `json:"resetsAt"`
			} `json:"rate_limit_info"`
		} `json:"event"`
	}
	if json.Unmarshal(raw, &f) != nil || f.Type != "llm_event" {
		return limitFrame{}
	}
	e := f.Event
	switch {
	case e.Type == "assistant" && e.Error == "rate_limit":
		return limitFrame{Hit: true}
	case e.Type == "assistant" && e.Delta.Type == "text_delta":
		return limitFrame{Text: e.Delta.Text}
	case e.Type == "rate_limit_event" && e.Info.Status == "rejected" && e.Info.ResetsAt > 0:
		return limitFrame{ResetsAt: e.Info.ResetsAt}
	}
	return limitFrame{}
}

// providerLimit says whether a session's turn ended on the provider's usage
// limit, and when the provider says it lifts.
type providerLimit struct {
	Hit    bool
	Resets string
}

const maxResetsRunes = 64

// sessionLimit reads one session's frames. The reset comes from the last
// rejected rate_limit_event; when none arrived, from the "resets ..." tail of
// the first reply text after the hit.
func sessionLimit[F interface{ limitOf() (string, limitFrame) }](fs []F, sid string) providerLimit {
	hit := -1
	var resetsAt int64
	for i, f := range fs {
		id, l := f.limitOf()
		if id != sid {
			continue
		}
		if l.Hit && hit < 0 {
			hit = i
		}
		if l.ResetsAt > 0 {
			resetsAt = l.ResetsAt
		}
	}
	if hit < 0 {
		return providerLimit{}
	}
	if resetsAt > 0 {
		return providerLimit{Hit: true, Resets: time.Unix(resetsAt, 0).UTC().Format(time.RFC3339)}
	}
	for _, f := range fs[hit:] {
		id, l := f.limitOf()
		if id != sid || l.Text == "" {
			continue
		}
		return providerLimit{Hit: true, Resets: resetsFromText(l.Text)}
	}
	return providerLimit{Hit: true}
}

func resetsFromText(text string) string {
	const marker = "resets "
	i := strings.LastIndex(text, marker)
	if i < 0 {
		return ""
	}
	tail := text[i+len(marker):]
	if nl := strings.IndexByte(tail, '\n'); nl >= 0 {
		tail = tail[:nl]
	}
	tail = strings.TrimSpace(tail)
	if r := []rune(tail); len(r) > maxResetsRunes {
		tail = string(r[:maxResetsRunes])
	}
	return tail
}

func (p providerLimit) detail() string {
	if p.Resets == "" {
		return "provider rate limit"
	}
	return "provider rate limit (resets " + p.Resets + ")"
}

// turnFail is the verdict for a turn-outcome failure: BLOCKED when the
// provider's limit ended the turn, FAIL otherwise.
func (p providerLimit) turnFail(id, d string) result {
	if p.Hit {
		return blocked(id, p.detail())
	}
	return result{id, stateFail, d}
}

func (f agentFrame) limitOf() (string, limitFrame) { return f.SessionID, f.Limit }
func (f dropFrame) limitOf() (string, limitFrame)  { return f.SessionID, f.Limit }
func (f cosFrame) limitOf() (string, limitFrame)   { return f.SessionID, f.Limit }
