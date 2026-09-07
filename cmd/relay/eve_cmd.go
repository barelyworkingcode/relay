package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// runEveCommand is `relay eve`'s dispatcher, mirroring runLoginCommand:
// `enrol` is the one subcommand today (docs/eve-passkey-enrolment.md), and
// this shape leaves room for a future one without a rewrite.
func runEveCommand(args []string) {
	runSubcommands("eve", []cliSubcommand{
		{"enrol", func(_ []string) { eveEnrol() }},
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
