package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
)

const (
	numbersProfile = "Verify Numbers"
	numbersMCP     = "devboxverify-numbers"
)

type numbersSave struct {
	Resp     frontendResponse
	Stored   string
	Widening bool
}

type numbersRun struct {
	Found, Granted bool
	Before         string
	Save, Restore  numbersSave
}

func runContextNumberResave(ctx context.Context, e env) result {
	const id = "context-number-resave"
	token, res, ok := runCredential(e, id)
	if !ok {
		return res
	}
	recs, err := grantRecords(ctx, e)
	if err != nil {
		return blocked(id, err.Error())
	}
	var r numbersRun
	g := findGrant(recs, numbersProfile)
	if g != nil {
		r.Found = true
		if m := g.mcpRow(numbersMCP); m != nil {
			r.Granted, r.Before = true, m.Scope["n"]
		}
	}
	if !r.Found || !r.Granted || r.Before != "1.0" {
		return classifyContextNumber(r)
	}
	baseline, err := newestConfigChangeID(ctx, e, g.ID)
	if err != nil {
		return blocked(id, err.Error())
	}
	var readErr error
	r.Save, baseline, readErr = saveNumber(ctx, e, token, g.ID, "1", baseline)
	if r.Save.Resp.Status == http.StatusOK {
		var restoreErr error
		r.Restore, _, restoreErr = saveNumber(ctx, e, token, g.ID, "1.0", baseline)
		if readErr == nil {
			readErr = restoreErr
		}
	}
	if readErr != nil {
		return blocked(id, "reading back after a save: "+readErr.Error())
	}
	return classifyContextNumber(r)
}

func saveNumber(ctx context.Context, e env, token, profileID, n, baseline string) (numbersSave, string, error) {
	// This is deliberate: the bodies are literal text because the fix is about
	// 1 and 1.0 being the same number, and json.Marshal would print both as 1.
	body := []byte(`{"context":{"` + numbersMCP + `":{"n":` + n + `}}}`)
	s := numbersSave{Resp: frontendDo(ctx, e, token, http.MethodPut, "/api/projects/"+profileID, body)}
	if s.Resp.Status != http.StatusOK {
		return s, baseline, nil
	}
	newest, err := newestConfigChangeID(ctx, e, profileID)
	if err != nil {
		return s, baseline, err
	}
	s.Widening = newest != baseline
	recs, err := grantRecords(ctx, e)
	if err != nil {
		return s, newest, err
	}
	if g := findGrant(recs, numbersProfile); g != nil {
		if m := g.mcpRow(numbersMCP); m != nil {
			s.Stored = m.Scope["n"]
		}
	}
	return s, newest, nil
}

func newestConfigChangeID(ctx context.Context, e env, subject string) (string, error) {
	out, err := exec.CommandContext(ctx, e.RelayBin, "audit", "--event", "config_change", "--grep", subject, "--json", "--tail", "1").Output()
	if err != nil {
		return "", fmt.Errorf("relay audit config_change: %w", err)
	}
	line := bytes.TrimSpace(out)
	if len(line) == 0 {
		return "", nil
	}
	var row struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(line, &row); err != nil {
		return "", fmt.Errorf("relay audit printed unreadable JSON: %w", err)
	}
	return row.ID, nil
}

func classifyContextNumber(r numbersRun) result {
	const id = "context-number-resave"
	fail := func(d string) result { return result{id, stateFail, d} }
	switch {
	case !r.Found:
		return blocked(id, "no access profile "+numbersProfile+"; set up P6")
	case !r.Granted:
		return blocked(id, numbersProfile+" does not grant "+numbersMCP+"; set up P6")
	case r.Before != "1.0":
		return blocked(id, fmt.Sprintf("%s holds n = %s, want 1.0; run the P6 restore step", numbersProfile, r.Before))
	}
	if res, refused := frontendRefusal(id, "saving n = 1", r.Save.Resp); refused {
		return res
	}
	switch {
	case r.Save.Widening:
		return fail("saving n = 1 over 1.0 recorded a config_change")
	case r.Save.Stored != "1":
		return fail(fmt.Sprintf("after saving n = 1 the profile holds %q", r.Save.Stored))
	}
	if res, refused := frontendRefusal(id, "restoring n = 1.0", r.Restore.Resp); refused {
		return fail(res.Detail + "; " + numbersProfile + " holds n = 1, run the P6 restore step")
	}
	switch {
	case r.Restore.Widening:
		return fail("restoring n = 1.0 recorded a config_change")
	case r.Restore.Stored != "1.0":
		return fail(fmt.Sprintf("after restoring n = 1.0 the profile holds %q", r.Restore.Stored))
	}
	return result{id, statePass, "saved n = 1 over 1.0 and restored, no prompt and no config_change"}
}
