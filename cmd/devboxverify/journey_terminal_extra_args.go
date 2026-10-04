package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	extraArgsID         = "terminal-extra-args"
	extraArgsTemplateID = "devboxverify-extra-args"
	extraArgMarker      = "devboxverify-extra-arg-marker"
	// extraArgPrefix is printed by the template script. The marker is never
	// in the template, so it reaches the log only through the launch's extraArgs.
	extraArgPrefix = "extra-arg:"

	extraArgsExitWait = 10 * time.Second
	extraArgsLogWait  = 5 * time.Second
)

type extraArgsRun struct {
	PreDelete, Template, Create frontendResponse
	TermID, Project             string
	List                        frontendResponse
	Stopped                     bool
	ExitCode                    int
	Log                         frontendResponse
}

func extraArgsTemplateBody(nonce string) []byte {
	return jsonBody(map[string]any{
		"id":      extraArgsTemplateID,
		"name":    "devboxverify extra args " + nonce,
		"command": "/bin/sh",
		"args":    []string{"-c", `printf 'extra-arg:%s\n' "$1"`, extraArgsTemplateID},
	})
}

// stoppedExitCode reads the row for id. The exit code is omitted when 0, so a
// stopped row without it means 0.
func stoppedExitCode(body []byte, id string) (stopped bool, exit int) {
	var v struct {
		Terminals []struct {
			ID       string `json:"id"`
			State    string `json:"state"`
			ExitCode int    `json:"exitCode"`
		} `json:"terminals"`
	}
	_ = json.Unmarshal(body, &v)
	for _, t := range v.Terminals {
		if t.ID == id {
			return t.State == "stopped", t.ExitCode
		}
	}
	return false, 0
}

func runTerminalExtraArgs(ctx context.Context, e env) result {
	launch, run, res, ok := screenCreds(e, extraArgsID)
	if !ok {
		return res
	}
	if e.Run == nil || e.Run.GrantProjectID == "" {
		return blocked(extraArgsID, "no Verify Grant project: "+grantPosID+" did not pass")
	}
	tmplPath := "/api/terminal/templates/" + extraArgsTemplateID
	r := extraArgsRun{Project: "Verify Grant"}
	r.PreDelete = frontendDo(ctx, e, run, http.MethodDelete, tmplPath, nil)
	if r.PreDelete.Status != http.StatusNoContent && r.PreDelete.Status != http.StatusNotFound {
		return classifyTerminalExtraArgs(r)
	}
	// Whatever happens next, the template does not outlive the run.
	defer func() {
		frontendDo(context.WithoutCancel(ctx), e, run, http.MethodDelete, tmplPath, nil)
	}()
	r.Template = frontendDo(ctx, e, run, http.MethodPost, "/api/terminal/templates", extraArgsTemplateBody(e.Nonce))
	if r.Template.Status != http.StatusCreated {
		return classifyTerminalExtraArgs(r)
	}
	body := jsonBody(map[string]any{
		"templateId": extraArgsTemplateID, "projectId": e.Run.GrantProjectID, "name": "verify-" + e.Nonce + "-args",
		"cols": 120, "rows": 40, "extraArgs": []string{extraArgMarker},
	})
	r.Create = frontendDoTimeout(ctx, e, launch, http.MethodPost, "/api/terminals", body, 30*time.Second)
	var created struct {
		TerminalID string `json:"terminalId"`
	}
	_ = json.Unmarshal(r.Create.Body, &created)
	if r.TermID = created.TerminalID; r.Create.Status != http.StatusCreated || r.TermID == "" {
		return classifyTerminalExtraArgs(r)
	}
	defer func() {
		frontendDo(context.WithoutCancel(ctx), e, run, http.MethodDelete, "/api/terminals/"+r.TermID, nil)
	}()
	for deadline := time.Now().Add(extraArgsExitWait); ; {
		r.List = frontendDo(ctx, e, run, http.MethodGet, "/api/terminals", nil)
		if r.List.Status != http.StatusOK {
			break
		}
		if r.Stopped, r.ExitCode = stoppedExitCode(r.List.Body, r.TermID); r.Stopped || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	for deadline := time.Now().Add(extraArgsLogWait); ; {
		r.Log = frontendDo(ctx, e, run, http.MethodGet, "/api/terminals/"+r.TermID+"/log", nil)
		if r.Log.Status == http.StatusOK && strings.Contains(string(r.Log.Body), extraArgPrefix+extraArgMarker) || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return classifyTerminalExtraArgs(r)
}

func classifyTerminalExtraArgs(r extraArgsRun) result {
	const id = extraArgsID
	fail := func(d string) result { return result{id, stateFail, d} }
	unauthorized := func(rs ...frontendResponse) bool {
		for _, x := range rs {
			if x.Status == http.StatusUnauthorized {
				return true
			}
		}
		return false
	}
	switch {
	case unauthorized(r.PreDelete, r.Template, r.List, r.Log):
		return blocked(id, "run credential refused (401)")
	case r.PreDelete.Status != http.StatusNoContent && r.PreDelete.Status != http.StatusNotFound:
		return fail(fmt.Sprintf("DELETE /api/terminal/templates/%s status %d: %s", extraArgsTemplateID, r.PreDelete.Status, r.PreDelete.Error))
	case r.Template.Status != http.StatusCreated:
		return fail(fmt.Sprintf("POST /api/terminal/templates status %d: %s", r.Template.Status, r.Template.Error))
	case r.Create.Status != http.StatusCreated || r.TermID == "":
		return launchRefusal(id, "/api/terminals", r.Project, r.Create)
	case r.List.Status != http.StatusOK:
		return fail(fmt.Sprintf("GET /api/terminals status %d", r.List.Status))
	case !r.Stopped:
		return fail("terminal still running 10 s after launch")
	case r.Log.Status != http.StatusOK:
		return fail(fmt.Sprintf("log status %d within 5 s", r.Log.Status))
	case !strings.Contains(string(r.Log.Body), extraArgPrefix+extraArgMarker):
		return fail("log lacks the extraArgs marker " + extraArgPrefix + extraArgMarker + "; last line: " + lastLine(string(r.Log.Body)))
	case r.ExitCode != 0:
		return fail(fmt.Sprintf("exit code %d", r.ExitCode))
	}
	return result{id, statePass, "terminal from a fixture template in " + r.Project + " printed its extraArgs marker and exited 0"}
}
