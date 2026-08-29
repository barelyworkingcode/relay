package main

import (
	"flag"
	"fmt"
)

// Login registration is anchored on the host, by the user who owns the
// config dir; nothing in this file is reachable over a socket (ADR-016
// decision 2). It runs in a separate process from the tray, so every read
// and write goes through the store rather than a cached settings view,
// matching enrol_cmd.go and credential_cmd.go. The mint, list and revoke
// themselves live in login_ops.go, which is also what the tray's own
// surfaces go through — the split enrolment.go and enrol_cmd.go use.
func runLoginCommand(args []string) {
	store := NewSettingsStore()
	runSubcommands("login", []cliSubcommand{
		{"enrol", func(_ []string) { loginEnrol(store) }},
		{"list", func(_ []string) { loginList(store) }},
		{"revoke", func(a []string) { loginRevoke(store, a) }},
	}, args)
}

func loginEnrol(store SettingsStore) {
	aud, closeAud := cliIssuanceAuditor(store)
	defer closeAud()

	plaintext, expires, err := mintLoginBootstrap(store)
	if err != nil {
		exitError("%v", err)
	}
	if err := recordBootstrapIssued(aud, expires, auditViaCLI); err != nil {
		refuseUnrecordedIssuance(err, "a login code was minted",
			"`relay login enrol` again once the audit log is writable; the unprinted anchor expires in "+bootstrapCodeTTL.String())
	}

	fmt.Printf("login code: %s\n", plaintext)
	fmt.Printf("  expires:   %s (valid for %s, single use)\n", expires, bootstrapCodeTTL)
	fmt.Println("  this code registers a passkey — it is NOT a password and is never accepted in place of one")
	if url := loginPageURL(); url != "" {
		fmt.Printf("  open %s and enter it to register a passkey\n", url)
	} else {
		fmt.Println("  open the relay login page (http://localhost:<RELAY_API_LISTEN port>/relay/login) and enter it to register a passkey")
	}
	fmt.Println("  this code is shown ONCE and is not recoverable")
}

func loginList(store SettingsStore) {
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

func loginRevoke(store SettingsStore, args []string) {
	fs := flag.NewFlagSet("login revoke", flag.ExitOnError)
	id := fs.String("id", "", "credential id of the passkey to revoke (required)")
	fs.Parse(args)

	aud, closeAud := cliIssuanceAuditor(store)
	defer closeAud()

	removed, err := revokePasskey(store, *id)
	if err != nil {
		exitError("%v", err)
	}
	if err := recordPasskeyRevoked(aud, removed, auditViaCLI); err != nil {
		warnUnrecordedRevocation(err, fmt.Sprintf("passkey %q (%s) was revoked", removed.Name, abbreviatePasskeyID(removed.ID)))
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
