package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"

	"github.com/barelyworkingcode/relay/internal/logging"
)

// The login verbs are the CLI door to the browser-session half of the Passkeys
// tab: LoginOps.Sessions and LoginOps.SignOut.

type loginSessionsResult struct {
	Sessions []loginSessionView `json:"sessions"`
}

type loginSignOutRequest struct {
	ID string `json:"id"`
}

type loginSignOutResult struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// adminLoginSessions is login.session.list. A read, so its event is written
// here: Sessions also feeds the Settings window.
func adminLoginSessions(ctx context.Context, r *appRouter, _ json.RawMessage) (_ json.RawMessage, err error) {
	ev := logging.BeginEvent(ctx, "login.session.list")
	defer func() { endEvent(ev, err) }()
	ops, err := requireLoginOps(r)
	if err != nil {
		return nil, err
	}
	sessions := ops.Sessions()
	ev.Set("count", len(sessions))
	return marshalAdminResult(loginSessionsResult{Sessions: sessions})
}

func adminLoginSignOut(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireLoginOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[loginSignOutRequest]("login.session.sign_out", args)
	if err != nil {
		return nil, err
	}
	removed, err := ops.SignOut(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	return marshalAdminResult(loginSignOutResult{ID: removed.ID, Name: removed.Name})
}

func loginSessions(args []string) {
	fs := flag.NewFlagSet("login sessions", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the sessions as JSON")
	fs.Parse(args)

	raw := adminCall("relay login sessions", "login.session.list", nil)
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var res loginSessionsResult
	decodeCLIResult(raw, &res)
	if len(res.Sessions) == 0 {
		fmt.Println("no browser sessions")
		return
	}
	w := newTabWriter()
	fmt.Fprintln(w, "NAME\tID\tCREATED\tEXPIRES")
	for _, s := range res.Sessions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Name, s.ID, s.Created, s.Expires)
	}
	w.Flush()
}

func loginSignOut(args []string) {
	fs := flag.NewFlagSet("login sign-out", flag.ExitOnError)
	id := fs.String("id", "", "session id from `relay login sessions` (required)")
	asJSON := fs.Bool("json", false, "print the signed-out session as JSON")
	fs.Parse(args)
	if *id == "" {
		exitError("--id is required")
	}

	raw := adminCall("relay login sign-out", "login.session.sign_out", loginSignOutRequest{ID: *id})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var res loginSignOutResult
	decodeCLIResult(raw, &res)
	fmt.Printf("signed out %s (%s)\n", res.Name, res.ID)
}
