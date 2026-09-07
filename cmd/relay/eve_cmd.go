package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
)

// runEveCommand is `relay eve`'s dispatcher, mirroring runLoginCommand:
// `list` reads settings.json directly, like `relay login list`, and works
// with the tray stopped; `enrol` and `revoke` are brokered over admin_op
// because this process holds no gate.
func runEveCommand(args []string) {
	store := config.NewSettingsStore()
	runSubcommands("eve", []cliSubcommand{
		{"enrol", func(_ []string) { eveEnrol() }},
		{"list", func(_ []string) { eveList(store) }},
		{"revoke", eveRevoke},
	}, args)
}

// eveEnrol brokers eve.enrolment.open over admin_op, exactly as loginEnrol
// brokers login.bootstrap.mint: this process holds no gate, so it dials the
// running tray and lets EveEnrolmentOps -- the same core the tray's own
// "Allow Eve Passkey Enrolment…" item calls -- do the work.
func eveEnrol() {
	client := requireService("relay eve enrol")
	raw, err := client.AdminOp("eve.enrolment.open", nil)
	if err != nil {
		exitError("%s", adminOpErrorText(err))
	}
	var view eveEnrolmentStatusView
	if err := json.Unmarshal(raw, &view); err != nil {
		exitError("parse response: %v", err)
	}

	local := view.Expires
	if at, perr := time.Parse(time.RFC3339, view.Expires); perr == nil {
		local = at.Local().Format("15:04:05")
	}
	fmt.Printf("eve passkey enrolment open until %s (%s, single use)\n", local, eveEnrolmentTTL)
	fmt.Println("  on the new browser, open Eve, and tap \"Add this browser\"")
}

// eveList reads relay's eve-passkey mirror straight off disk, the same
// door `relay login list` uses for relay's own passkeys -- it needs no
// running tray.
func eveList(store config.SettingsStore) {
	views := evePasskeyViews(store.Get())
	if len(views) == 0 {
		fmt.Println("no eve passkeys reported")
		return
	}

	w := newTabWriter()
	fmt.Fprintln(w, "LABEL\tCREDENTIAL ID\tCREATED\tLAST USED\tSTATUS")
	for _, p := range views {
		status := "-"
		if p.RevocationPending {
			status = "revocation pending"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Label, p.Short, p.Created, p.LastUsed, status)
	}
	w.Flush()
}

// eveRevoke brokers eve.passkey.revoke over admin_op: relay records the
// revocation as pending and eve applies it on its own next poll or login
// check (docs/eve-passkey-enrolment.md decision 10).
func eveRevoke(args []string) {
	fs := flag.NewFlagSet("eve revoke", flag.ExitOnError)
	id := fs.String("id", "", "credential id of the eve passkey to revoke (required)")
	fs.Parse(args)

	client := requireService("relay eve revoke")
	body, err := json.Marshal(evePasskeyRevokeRequest{ID: *id})
	if err != nil {
		exitError("%v", err)
	}
	raw, err := client.AdminOp("eve.passkey.revoke", body)
	if err != nil {
		exitError("%s", adminOpErrorText(err))
	}
	var rec config.EvePasskeyRevocation
	if err := json.Unmarshal(raw, &rec); err != nil {
		exitError("parse response: %v", err)
	}

	fmt.Printf("eve passkey %s: revocation pending\n", abbreviatePasskeyID(rec.ID))
	fmt.Println("  it will stop working on its next use")
	fmt.Println("  eve signs out every session that passkey minted when it applies the revocation")
}
