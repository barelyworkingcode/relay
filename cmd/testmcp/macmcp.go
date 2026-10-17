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
	"strconv"
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
	MessageID        json.RawMessage `json:"message_id"`
	Account          string          `json:"account"`
	Mailbox          string          `json:"mailbox"`
	Query            string          `json:"query"`
	Limit            int             `json:"limit"`
	BodyScanLimit    int             `json:"body_scan_limit"`
	SearchRecipients bool            `json:"search_recipients"`
	SearchBody       bool            `json:"search_body"`
	To               json.RawMessage `json:"to"`
	Subject          *string         `json:"subject"`
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
		search := req.Name == "mail_search"
		if search && strings.TrimSpace(a.Query) == "" {
			return toolText("query is required", true)
		}
		if a.Mailbox == "" && !search && !sc.mediated {
			a.Mailbox = "INBOX"
		}
		if res := p.outOfScope(sc, a); res != nil {
			return res
		}
		return toolJSON(p.scan(sc, a, search))
	case "mail_get_email":
		return p.getEmail(sc, a)
	case "mail_send":
		return p.send(sc, a)
	case "web_fetch":
		return webFetch(req.Arguments)
	}
	return toolText("testmcp macmcp persona: no answer recorded for "+req.Name, true)
}

func (p *persona) outOfScope(sc scope, a mailArgs) json.RawMessage {
	switch {
	case a.Account != "" && !sc.account(a.Account):
		return scopeViolation("account " + a.Account + " is outside this client's mail_accounts")
	case !isAll(a.Mailbox) && !p.mailboxInScope(sc, a):
		return scopeViolation("mailbox " + a.Mailbox + " is outside this client's mail_mailboxes")
	}
	return nil
}

// excludedBoxes are what a mailbox "all" scan leaves out of every account.
var excludedBoxes = []string{"Drafts", "Trash", "Junk", "Outbox"}

func isAll(mailbox string) bool { return mailbox == "" || strings.EqualFold(mailbox, "all") }

// scan answers mail_get_emails and mail_search the way macMCP does: an object
// holding the messages and the scan-coverage fields.
func (p *persona) scan(sc scope, a mailArgs, search bool) map[string]any {
	all := isAll(a.Mailbox)
	var scanned []string
	var excluded []string
	var inScan []mailMessage
	for _, acc := range p.accounts {
		if !sc.account(acc.Name) || a.Account != "" && acc.Name != a.Account {
			continue
		}
		for _, box := range acc.Mailboxes {
			switch {
			case !sc.mailbox(box):
				continue
			case all && slices.Contains(excludedBoxes, box):
				excluded = append(excluded, acc.Name+":"+box)
				continue
			case !all && !strings.EqualFold(box, a.Mailbox):
				continue
			}
			scanned = append(scanned, acc.Name+":"+box)
			for _, m := range p.messages {
				if m.Account == acc.Name && m.Mailbox == box {
					inScan = append(inScan, m)
				}
			}
		}
	}
	sort.Strings(scanned)
	sort.Strings(excluded)
	sort.SliceStable(inScan, func(i, j int) bool { return inScan[i].Date.After(inScan[j].Date) })

	hits := inScan
	out := map[string]any{"messages_scanned": len(inScan), "scan_complete": true,
		"scanned_mailboxes": nonNil(scanned), "skipped_mailboxes": []string{}}
	if search {
		hits = nil
		var rest []mailMessage
		q := strings.ToLower(a.Query)
		for _, m := range inScan {
			if matches(m, q, a.SearchRecipients) {
				hits = append(hits, m)
			} else {
				rest = append(rest, m)
			}
		}
		if a.SearchBody {
			limit := a.BodyScanLimit
			if limit <= 0 {
				limit = 25
			}
			limit = min(limit, 200)
			read := rest[:min(limit, len(rest))]
			bodyHits := 0
			for _, m := range read {
				if strings.Contains(strings.ToLower(m.Body), q) {
					hits = append(hits, m)
					bodyHits++
				}
			}
			bs := map[string]any{"bodies_read": len(read), "body_matches": bodyHits, "body_scan_complete": len(read) == len(rest),
				"body_scan_limit": limit}
			if short := limit - len(read); short > 0 {
				bs["body_scan_shortfall"] = short
				bs["body_scan_shortfall_note"] = fmt.Sprintf("There was room to read %d message body(ies) and only %d message(s) to read them from.", limit, len(read))
			}
			out["body_search"] = bs
			sort.SliceStable(hits, func(i, j int) bool { return hits[i].Date.After(hits[j].Date) })
		}
		out["total_matches"] = len(hits)
		if all && !sc.mediated {
			out["excluded_mailboxes"] = nonNil(excluded)
		}
	} else {
		out["total_messages"] = len(hits)
		if all && !sc.mediated {
			out["excluded_mailboxes"] = nonNil(excluded)
		}
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 10
	}
	out["truncated"] = len(hits) > limit
	msgs := []map[string]any{}
	for _, m := range hits[:min(limit, len(hits))] {
		msgs = append(msgs, map[string]any{"id": fmt.Sprint(m.ID), "account": m.Account, "mailbox": m.Mailbox,
			"subject": m.Subject, "sender": m.From, "date_received": jsDate(m.Date), "read": m.Seen})
	}
	out["messages"] = msgs
	return out
}

// jsDate renders a time the way macMCP reports Mail's dates (a JavaScript
// Date string).
func jsDate(t time.Time) string {
	zone := map[string]string{"PDT": "Pacific Daylight Time", "PST": "Pacific Standard Time", "UTC": "Coordinated Universal Time"}
	abbr, _ := t.Zone()
	name := abbr
	if n, ok := zone[abbr]; ok {
		name = n
	}
	return t.Format("Mon Jan 02 2006 15:04:05 GMT-0700") + " (" + name + ")"
}

func matches(m mailMessage, q string, recipients bool) bool {
	fields := []string{m.Subject, m.From}
	if recipients {
		fields = append(fields, m.To...)
		fields = append(fields, m.Cc...)
	}
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// messageRef reads message_id as macMCP takes it: a number, a numeric string,
// or an RFC Message-ID with or without its angle brackets.
func messageRef(raw json.RawMessage) (id int, rfc string, ok bool) {
	if len(raw) == 0 {
		return 0, "", false
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		i, err := strconv.Atoi(n.String())
		return i, "", err == nil
	}
	var s string
	if json.Unmarshal(raw, &s) != nil || strings.TrimSpace(s) == "" {
		return 0, "", false
	}
	s = strings.TrimSpace(s)
	if i, err := strconv.Atoi(s); err == nil {
		return i, "", true
	}
	return 0, strings.Trim(s, "<>"), true
}

func (p *persona) getEmail(sc scope, a mailArgs) json.RawMessage {
	id, rfc, ok := messageRef(a.MessageID)
	if !ok {
		return toolText("message_id is required: a numeric id or an RFC Message-ID", true)
	}
	label := strings.Trim(string(a.MessageID), `"`)
	for _, m := range p.messages {
		if rfc == "" && m.ID != id || rfc != "" && strings.Trim(m.MessageID, "<>") != rfc {
			continue
		}
		if !sc.account(m.Account) || !sc.mailbox(m.Mailbox) {
			return scopeViolation("message " + label + " is outside this client's scope")
		}
		return toolJSON(map[string]any{"id": fmt.Sprint(m.ID), "rfc_message_id": strings.Trim(m.MessageID, "<>"),
			"account": m.Account, "mailbox": m.Mailbox, "subject": m.Subject, "sender": m.From,
			"to": nonNil(m.To), "cc": nonNil(m.Cc), "date_received": jsDate(m.Date), "date_sent": jsDate(m.Date),
			"read": m.Seen, "body": m.Body, "attachments": []any{}, "has_attachments": false,
			"message_size": len(m.Body), "structure": map[string]any{"depth": 1, "parsed_complete": true, "parts": 1},
			"fidelity": map[string]any{"complete": true, "bytes_measured": len(m.Body), "message_size": len(m.Body),
				"ambiguous_nul_bytes": 0, "line_endings": "lf", "complete_basis": "wire+terminated",
				"note": "The recorded body is complete."}})
	}
	return toolText("message not found with id: "+label, true)
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
	case len(a.To) == 0 || string(a.To) == "null" || a.Subject == nil:
		return toolText("to and subject are required", true)
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

// mailboxInScope matches a mailbox name case-insensitively, as macMCP does, so
// "inbox" is the same mailbox as "INBOX" for the scope check.
func (p *persona) mailboxInScope(sc scope, a mailArgs) bool {
	for _, acc := range p.accounts {
		for _, box := range acc.Mailboxes {
			if strings.EqualFold(box, a.Mailbox) && sc.mailbox(box) {
				return true
			}
		}
	}
	return sc.mailbox(a.Mailbox)
}
