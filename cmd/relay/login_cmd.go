package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/barelyworkingcode/relay/internal/config"
)

// Login registration is anchored on the host, by the user who owns the
// config dir. `enrol` and `revoke` are brokered (ADR-017 decision 2): this
// process holds no sealer (§5.4), so it dials the running tray over
// admin_op and lets LoginOps — the same core the Passkeys tab and the
// tray's own "Show Login Code..." item share — do the work. `list` is
// unaffected: it reads settings.json directly and keeps working with the
// tray stopped.
//
// ADR-016 decision 2 kept this subcommand specifically because the tray's
// menu is unreachable over SSH; ADR-017 §3.2 withdraws that affordance on
// purpose — a session that cannot show a presence prompt refuses here
// exactly as it does for every other gated operation, with no exemption.
func runLoginCommand(args []string) {
	store := config.NewSettingsStore()
	runSubcommands("login", []cliSubcommand{
		{"enrol", func(_ []string) { loginEnrol() }},
		{"list", func(_ []string) { loginList(store) }},
		{"revoke", loginRevoke},
	}, args)
}

func loginEnrol() {
	client := requireService("relay login enrol")
	raw, err := client.AdminOp("login.bootstrap.mint", nil)
	if err != nil {
		exitError("%s", adminOpErrorText(err))
	}
	var view loginCodeView
	if err := json.Unmarshal(raw, &view); err != nil {
		exitError("parse response: %v", err)
	}

	fmt.Printf("login code: %s\n", view.Code)
	fmt.Printf("  expires:   %s (valid for %s, single use)\n", view.Expires, bootstrapCodeTTL)
	fmt.Println("  this code registers a passkey — it is NOT a password and is never accepted in place of one")
	if view.URL != "" {
		fmt.Printf("  open %s and enter it to register a passkey\n", view.URL)
	} else {
		fmt.Println("  open the relay login page (http://localhost:<RELAY_API_LISTEN port>/relay/login) and enter it to register a passkey")
	}
	fmt.Println("  this code is shown ONCE and is not recoverable")
}

func loginList(store config.SettingsStore) {
	s := store.Get()

	if len(s.Passkeys) == 0 {
		fmt.Println("no passkeys registered")
		return
	}

	w := newTabWriter()
	fmt.Fprintln(w, "NAME\tCREDENTIAL ID\tCREATED\tSIGN COUNT")
	for _, p := range s.Passkeys {
		// Neither X nor Y is printed. They are a public key, not a secret,
		// but a listing has no legitimate use for them and printing them
		// would be the obvious place to start treating this output as
		// somewhere key material lives.
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", p.Name, abbreviatePasskeyID(p.ID), p.Created, p.SignCount)
	}
	w.Flush()
}

func loginRevoke(args []string) {
	fs := flag.NewFlagSet("login revoke", flag.ExitOnError)
	id := fs.String("id", "", "credential id of the passkey to revoke (required)")
	fs.Parse(args)

	client := requireService("relay login revoke")
	body, err := json.Marshal(loginPasskeyRevokeRequest{ID: *id})
	if err != nil {
		exitError("%v", err)
	}
	raw, err := client.AdminOp("login.passkey.revoke", body)
	if err != nil {
		exitError("%s", adminOpErrorText(err))
	}
	var removed config.Passkey
	if err := json.Unmarshal(raw, &removed); err != nil {
		exitError("parse response: %v", err)
	}

	fmt.Printf("revoked passkey %q\n", removed.Name)
	fmt.Printf("  id: %s\n", abbreviatePasskeyID(removed.ID))
	fmt.Println("  it can no longer complete a login assertion; nothing else was touched")
	// Stated because the two are separate records with separate lifetimes: a
	// browser that signed in before this revoke keeps its credential until it
	// expires, and an operator who read "revoked" as "signed out" would be
	// wrong for up to loginCredentialTTL.
	fmt.Println("  browser sessions this passkey already signed in are NOT ended — see `relay credential list`, or Settings → Passkeys")
}
