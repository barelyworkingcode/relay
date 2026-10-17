package main

// The macmcp persona (RELAY_TESTMCP_PERSONA=macmcp) stands in for macMCP so a
// verification stack can prove relay's grants, refusals and audit without
// macOS Automation. It serves initialize and tools/list byte for byte from a
// recording (RELAY_TESTMCP_RECORDING, written by devboxverify record-macmcp)
// and answers the mail reads from the devboxWorld checkout's data files
// (RELAY_TESTMCP_WORLD). It never runs a program and sends no Apple Events.
//
// A failure seen only against this persona is a mock defect until the
// nightly reproduces it against the real macMCP.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/jsonrpc"
)

func macmcpMode() bool { return os.Getenv("RELAY_TESTMCP_PERSONA") == "macmcp" }

// worldMailboxes mirrors devboxWorld's AUTO_MAILBOXES: every world account has
// these, plus the mailboxes its project file names.
var worldMailboxes = []string{"INBOX", "Sent", "Drafts", "Trash", "Junk", "Archive"}

type recording struct {
	Initialize json.RawMessage            `json:"initialize"`
	ToolsList  json.RawMessage            `json:"tools_list"`
	Answers    map[string]json.RawMessage `json:"answers"`
}

type mailAccount struct {
	Key, Name, Address string
	Mailboxes          []string
}

type mailMessage struct {
	ID        int // Mail's per-copy id: one message in two accounts is two ids
	MessageID string
	Account   string // account name
	Mailbox   string
	Subject   string
	From      string
	To, Cc    []string
	Date      time.Time
	Body      string
	Seen      bool
	Flagged   bool
}

type persona struct {
	rec      recording
	accounts []mailAccount
	messages []mailMessage
}

func loadPersona() (*persona, error) {
	recPath, worldDir := os.Getenv("RELAY_TESTMCP_RECORDING"), os.Getenv("RELAY_TESTMCP_WORLD")
	if recPath == "" || worldDir == "" {
		return nil, errors.New("the macmcp persona needs RELAY_TESTMCP_RECORDING and RELAY_TESTMCP_WORLD")
	}
	p := &persona{}
	if err := readJSON(recPath, &p.rec); err != nil {
		return nil, err
	}
	if len(p.rec.Initialize) == 0 || len(p.rec.ToolsList) == 0 {
		return nil, errors.New("recording holds no initialize or tools_list")
	}
	anchor, err := personaAnchor()
	if err != nil {
		return nil, err
	}
	return p, p.loadWorld(worldDir, anchor)
}

// personaAnchor is the world's day zero: RELAY_TESTMCP_ANCHOR (YYYY-MM-DD,
// the manifest's anchor) when set, else today in local time.
func personaAnchor() (time.Time, error) {
	if s := os.Getenv("RELAY_TESTMCP_ANCHOR"); s != "" {
		return time.ParseInLocation(time.DateOnly, s, time.Local)
	}
	y, m, d := time.Now().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local), nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

func (p *persona) loadWorld(dir string, anchor time.Time) error {
	var world struct {
		MailDomain string `json:"mail_domain"`
		Projects   []struct {
			Key, Name string
		} `json:"projects"`
	}
	if err := readJSON(filepath.Join(dir, "data", "world.json"), &world); err != nil {
		return err
	}
	byKey := map[string]*mailAccount{}
	for _, pr := range world.Projects {
		var proj struct {
			Mailboxes []string `json:"mailboxes"`
		}
		if err := readJSON(filepath.Join(dir, "data", "projects", pr.Key+".json"), &proj); err != nil {
			return err
		}
		boxes := slices.Concat(worldMailboxes, proj.Mailboxes)
		slices.Sort(boxes)
		p.accounts = append(p.accounts, mailAccount{Key: pr.Key, Name: pr.Name,
			Address: pr.Key + "@" + world.MailDomain, Mailboxes: slices.Compact(boxes)})
	}
	for i := range p.accounts {
		byKey[p.accounts[i].Key] = &p.accounts[i]
	}
	// A party is a world key string or {"name", "address"}.
	render := func(raw json.RawMessage) string {
		var key string
		if json.Unmarshal(raw, &key) == nil {
			if a := byKey[key]; a != nil {
				return fmt.Sprintf("%s <%s>", a.Name, a.Address)
			}
			return key
		}
		var np struct{ Name, Address string }
		_ = json.Unmarshal(raw, &np)
		return fmt.Sprintf("%s <%s>", np.Name, np.Address)
	}
	var mail struct {
		Threads []struct {
			Subject  string `json:"subject"`
			Messages []struct {
				ID     string            `json:"id"`
				From   json.RawMessage   `json:"from"`
				To     []json.RawMessage `json:"to"`
				Cc     []json.RawMessage `json:"cc"`
				Day    int               `json:"day"`
				Time   string            `json:"time"`
				Body   string            `json:"body"`
				Copies []struct {
					Account, Mailbox string
					Seen, Flagged    bool
				} `json:"copies"`
			} `json:"messages"`
		} `json:"threads"`
	}
	if err := readJSON(filepath.Join(dir, "data", "mail.json"), &mail); err != nil {
		return err
	}
	for _, th := range mail.Threads {
		for i, m := range th.Messages {
			subject := th.Subject
			if i > 0 {
				subject = "Re: " + subject
			}
			clock, err := time.Parse("15:04", m.Time)
			if err != nil {
				return fmt.Errorf("mail.json %s: time %q", m.ID, m.Time)
			}
			date := anchor.AddDate(0, 0, m.Day).Add(time.Duration(clock.Hour())*time.Hour + time.Duration(clock.Minute())*time.Minute)
			var to, cc []string
			for _, x := range m.To {
				to = append(to, render(x))
			}
			for _, x := range m.Cc {
				cc = append(cc, render(x))
			}
			for _, c := range m.Copies {
				a := byKey[c.Account]
				if a == nil {
					return fmt.Errorf("mail.json %s: copy in unknown account %q", m.ID, c.Account)
				}
				p.messages = append(p.messages, mailMessage{ID: len(p.messages) + 1,
					MessageID: "<" + m.ID + "@" + world.MailDomain + ">", Account: a.Name, Mailbox: c.Mailbox,
					Subject: subject, From: render(m.From), To: to, Cc: cc, Date: date, Body: m.Body,
					Seen: c.Seen, Flagged: c.Flagged})
			}
		}
	}
	return nil
}

// scope is what relay put in _meta. Mediated with an unset field means the
// field confines to nothing a tool may reach: macMCP fails closed, and so
// does this persona.
type scope struct {
	mediated            bool
	accounts, mailboxes []string
	accountsSet, boxSet bool
}

func readScope(meta json.RawMessage) scope {
	if len(meta) == 0 || string(meta) == "null" {
		return scope{}
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(meta, &m)
	s := scope{mediated: true}
	s.accounts, s.accountsSet = scopeList(m["mail_accounts"])
	s.mailboxes, s.boxSet = scopeList(m["mail_mailboxes"])
	return s
}

// scopeList reads an array of strings, or a string holding one. Absent,
// null, "" and anything else unreadable are unset.
func scopeList(raw json.RawMessage) ([]string, bool) {
	var list []string
	if json.Unmarshal(raw, &list) == nil && list != nil {
		return list, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" && json.Unmarshal([]byte(s), &list) == nil && list != nil {
		return list, true
	}
	return nil, false
}

func inList(list []string, v string) bool {
	return len(list) == 1 && list[0] == "*" || slices.Contains(list, v)
}

func (s scope) account(name string) bool { return !s.mediated || inList(s.accounts, name) }
func (s scope) mailbox(name string) bool { return !s.mediated || inList(s.mailboxes, name) }

// unset names the first restrict field a mediated call lacks.
func (s scope) unset() string {
	switch {
	case !s.mediated:
		return ""
	case !s.accountsSet:
		return "mail_accounts"
	case !s.boxSet:
		return "mail_mailboxes"
	}
	return ""
}

func toolText(text string, isErr bool) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": isErr})
	return b
}

func toolJSON(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // "Name <address>" stays readable in the text
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return toolText(strings.TrimSuffix(buf.String(), "\n"), false)
}

func scopeViolation(text string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"content": []map[string]string{{"type": "text", "text": text}},
		"isError": true, "_meta": map[string]bool{"scope_violation": true}})
	return b
}

func (p *persona) enumerate(params json.RawMessage) (json.RawMessage, *jsonrpc.Error) {
	var req struct {
		Field  string              `json:"field"`
		Values map[string][]string `json:"values"`
	}
	_ = json.Unmarshal(params, &req)
	type value struct {
		Value string `json:"value"`
		Label string `json:"label,omitempty"`
	}
	out := []value{}
	switch req.Field {
	case "mail_accounts":
		for _, a := range p.accounts {
			out = append(out, value{a.Name, a.Name})
		}
	case "mail_mailboxes":
		within := req.Values["mail_accounts"]
		seen := map[string]bool{}
		for _, a := range p.accounts {
			if len(within) > 0 && !inList(within, a.Name) {
				continue
			}
			for _, b := range a.Mailboxes {
				if !seen[b] {
					seen[b] = true
					out = append(out, value{b, b})
				}
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	default:
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "no enumerable field named " + req.Field}
	}
	b, _ := json.Marshal(map[string]any{"field": req.Field, "values": out})
	return b, nil
}

type mailArgs struct {
	ID                string `json:"id"`
	Account           string `json:"account"`
	Mailbox           string `json:"mailbox"`
	Query             string `json:"query"`
	Limit             int    `json:"limit"`
	IncludeRecipients bool   `json:"include_recipients"`
	IncludeBody       bool   `json:"include_body"`
}

func (p *persona) call(params json.RawMessage) json.RawMessage {
	var req struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      json.RawMessage `json:"_meta"`
	}
	_ = json.Unmarshal(params, &req)
	sc := readScope(req.Meta)
	if strings.HasPrefix(req.Name, "mail_") {
		if f := sc.unset(); f != "" {
			return scopeViolation(f + " is not set for this client")
		}
	}
	var a mailArgs
	if len(req.Arguments) > 0 && json.Unmarshal(req.Arguments, &a) != nil {
		return toolText("arguments must be an object", true)
	}
	switch req.Name {
	case "mail_list_accounts":
		names := []string{}
		for _, acc := range p.accounts {
			if sc.account(acc.Name) {
				names = append(names, acc.Name)
			}
		}
		return toolJSON(names)
	case "mail_list_mailboxes":
		if a.Account != "" && !sc.account(a.Account) {
			return scopeViolation("account " + a.Account + " is outside this client's mail_accounts")
		}
		out := map[string][]string{}
		for _, acc := range p.accounts {
			if !sc.account(acc.Name) || a.Account != "" && acc.Name != a.Account {
				continue
			}
			boxes := []string{}
			for _, b := range acc.Mailboxes {
				if sc.mailbox(b) {
					boxes = append(boxes, b)
				}
			}
			out[acc.Name] = boxes
		}
		return toolJSON(out)
	case "mail_get_emails", "mail_search":
		if req.Name == "mail_search" && strings.TrimSpace(a.Query) == "" {
			return toolText("query is required", true)
		}
		if res := p.outOfScope(sc, a); res != nil {
			return res
		}
		return toolJSON(p.summaries(sc, a, req.Name == "mail_search"))
	case "mail_get_email":
		for _, m := range p.messages {
			if fmt.Sprint(m.ID) != a.ID {
				continue
			}
			if !sc.account(m.Account) || !sc.mailbox(m.Mailbox) {
				return scopeViolation("message " + a.ID + " is outside this client's scope")
			}
			return toolJSON(map[string]any{"id": fmt.Sprint(m.ID), "message_id": m.MessageID, "account": m.Account,
				"mailbox": m.Mailbox, "subject": m.Subject, "from": m.From, "to": nonNil(m.To), "cc": nonNil(m.Cc),
				"date": m.Date.Format(time.RFC3339), "read": m.Seen, "flagged": m.Flagged, "body": m.Body,
				"attachments": []any{}})
		}
		return toolText("no message with id "+a.ID, true)
	case "mail_send":
		return p.send(sc, a)
	}
	return toolText("testmcp macmcp persona: no answer recorded for "+req.Name, true)
}

func (p *persona) outOfScope(sc scope, a mailArgs) json.RawMessage {
	switch {
	case a.Account != "" && !sc.account(a.Account):
		return scopeViolation("account " + a.Account + " is outside this client's mail_accounts")
	case a.Mailbox != "" && !sc.mailbox(a.Mailbox):
		return scopeViolation("mailbox " + a.Mailbox + " is outside this client's mail_mailboxes")
	}
	return nil
}

func (p *persona) summaries(sc scope, a mailArgs, search bool) []map[string]any {
	q := strings.ToLower(a.Query)
	var hits []mailMessage
	for _, m := range p.messages {
		switch {
		case !sc.account(m.Account) || !sc.mailbox(m.Mailbox):
			continue
		case a.Account != "" && m.Account != a.Account, a.Mailbox != "" && m.Mailbox != a.Mailbox:
			continue
		case search && !matches(m, q, a):
			continue
		}
		hits = append(hits, m)
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Date.After(hits[j].Date) })
	limit := a.Limit
	if limit <= 0 {
		limit = 20
	}
	out := []map[string]any{}
	for _, m := range hits[:min(limit, len(hits))] {
		out = append(out, map[string]any{"id": fmt.Sprint(m.ID), "account": m.Account, "mailbox": m.Mailbox,
			"subject": m.Subject, "from": m.From, "date": m.Date.Format(time.RFC3339), "read": m.Seen, "flagged": m.Flagged})
	}
	return out
}

func matches(m mailMessage, q string, a mailArgs) bool {
	fields := []string{m.Subject, m.From}
	if a.IncludeRecipients {
		fields = append(fields, m.To...)
		fields = append(fields, m.Cc...)
	}
	if a.IncludeBody {
		fields = append(fields, m.Body)
	}
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// send delivers nothing. Inside the client's scope it answers what the
// recording says the real macMCP answers for a sent message.
func (p *persona) send(sc scope, a mailArgs) json.RawMessage {
	account := a.Account
	if account == "" {
		for _, acc := range p.accounts {
			if sc.account(acc.Name) {
				account = acc.Name
				break
			}
		}
	}
	switch {
	case account == "":
		return toolText("no account in scope to send from", true)
	case !sc.account(account):
		return scopeViolation("account " + account + " is outside this client's mail_accounts")
	}
	if r, ok := p.rec.Answers["mail_send"]; ok {
		return r
	}
	return toolText("testmcp macmcp persona: no mail_send answer recorded", true)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
