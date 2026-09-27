package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os/exec"
	"slices"
	"strings"
)

const (
	staleID      = "stale-derived-access-edit"
	staleProfile = "Verify Stale"
	staleMcp     = "devboxverify-scope"
)

type staleState struct {
	Found, Remote, Granted, Registered, FileDirs bool
	MailAccounts                                 []string
}

func runStaleDerivedEdit(ctx context.Context, e env) result {
	before, recordID, res, ok := readStaleState(ctx, e)
	if !ok {
		return res
	}
	if res, ok := staleFixtureReady(before); !ok {
		return res
	}
	token, res, ok := configureCredential(staleID)
	if !ok {
		return res
	}
	body, _ := json.Marshal(map[string]map[string]string{"access": {staleMcp: "read"}})
	r := frontendDo(ctx, e, token, http.MethodPut, "/api/projects/"+url.PathEscape(recordID), body)
	var after staleState
	if r.Status == http.StatusOK {
		if after, _, res, ok = readStaleState(ctx, e); !ok {
			return res
		}
	}
	return classifyStaleDerivedEdit(before, r, after)
}

func readStaleState(ctx context.Context, e env) (s staleState, recordID string, res result, ok bool) {
	rs, err := grantRecords(ctx, e)
	if err != nil {
		return s, "", blocked(staleID, err.Error()), false
	}
	table, err := exec.CommandContext(ctx, e.RelayBin, "mcp", "list").Output()
	if err != nil {
		return s, "", blocked(staleID, "relay mcp list failed"), false
	}
	s.Registered = mcpListed(string(table), staleMcp)
	g := findGrant(rs, staleProfile)
	if g == nil {
		return s, "", result{}, true
	}
	s.Found, s.Remote = true, g.Kind == "access profile"
	row := g.mcpRow(staleMcp)
	if row == nil {
		return s, g.ID, result{}, true
	}
	s.Granted = true
	_, s.FileDirs = row.Scope["file_dirs"]
	if raw, has := row.Scope["mail_accounts"]; has {
		_ = json.Unmarshal([]byte(raw), &s.MailAccounts)
	}
	return s, g.ID, result{}, true
}

// mcpListed matches whole ids only: devboxverify-scope must not be found on
// a row for devboxverify-scope-old, nor "ID" on the header.
func mcpListed(table, id string) bool {
	for i, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		if i == 0 && len(f) >= 2 && f[0] == "ID" && f[1] == "NAME" {
			continue
		}
		if len(f) > 0 && f[0] == id {
			return true
		}
	}
	return false
}

func staleFixtureReady(s staleState) (result, bool) {
	switch {
	case !s.Found:
		return blocked(staleID, "no access profile "+staleProfile+"; set up P5"), false
	case !s.Remote:
		return blocked(staleID, staleProfile+" is not an access profile; set up P5"), false
	case !s.Granted:
		return blocked(staleID, staleProfile+" does not grant "+staleMcp+"; set up P5"), false
	case !s.Registered:
		return blocked(staleID, staleMcp+" is not registered; set up P5"), false
	case !s.FileDirs:
		return result{staleID, stateNotRun, staleProfile + " holds no stale file_dirs; fixture spent, re-arm P5"}, false
	}
	return result{}, true
}

func classifyStaleDerivedEdit(before staleState, r frontendResponse, after staleState) result {
	if res, ok := staleFixtureReady(before); !ok {
		return res
	}
	fail := func(d string) result { return result{staleID, stateFail, d} }
	if r.Status == http.StatusBadRequest && strings.Contains(r.Error, "is derived by relay from the project's path") {
		return fail("access-only edit refused over the stale derived file_dirs: " + r.Error)
	}
	if res, refused := frontendRefusal(staleID, "access-only edit", r); refused {
		return res
	}
	switch {
	case after.FileDirs:
		return fail("access-only edit accepted but the stale file_dirs was kept")
	case !slices.Equal(before.MailAccounts, after.MailAccounts):
		return fail("access-only edit changed mail_accounts from " + strings.Join(before.MailAccounts, ",") + " to " + strings.Join(after.MailAccounts, ","))
	}
	return result{staleID, statePass, "access-only edit accepted; stale file_dirs dropped, mail_accounts kept"}
}
