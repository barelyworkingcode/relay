package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
)

// Login registration is anchored on the host, by the user who owns the
// config dir; nothing in this file is reachable over a socket (ADR-016
// decision 2). It runs in a separate process from the tray, so every read
// and write goes through the store rather than a cached settings view,
// matching enrol_cmd.go and credential_cmd.go.
func runLoginCommand(args []string) {
	store := NewSettingsStore()
	runSubcommands("login", []cliSubcommand{
		{"enrol", func(_ []string) { loginEnrol(store) }},
		{"list", func(_ []string) { loginList(store) }},
		{"revoke", func(a []string) { loginRevoke(store, a) }},
	}, args)
}

// mintLoginBootstrap wraps mintBootstrapCode in the store.With every CLI
// mint here goes through, matching mintAPICredential.
func mintLoginBootstrap(store SettingsStore) (string, string, error) {
	var plaintext, expires string
	var mintErr error
	if err := store.With(func(s *Settings) {
		plaintext, mintErr = mintBootstrapCode(s)
		if mintErr == nil {
			expires = s.LoginBootstrap.Expires
		}
	}); err != nil {
		return "", "", fmt.Errorf("save settings: %w", err)
	}
	if mintErr != nil {
		return "", "", mintErr
	}
	return plaintext, expires, nil
}

// revokePasskey resolves and removes inside one store.With, matching
// revokeAPICredential: a separate Get() then With() is a TOCTOU window on a
// file two processes write.
func revokePasskey(store SettingsStore, id string) (Passkey, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Passkey{}, errors.New("a passkey id is required")
	}

	var removed Passkey
	var found bool
	if err := store.With(func(s *Settings) {
		for i := range s.Passkeys {
			if s.Passkeys[i].ID == id {
				removed = s.Passkeys[i]
				found = true
				s.Passkeys = slices.Delete(s.Passkeys, i, i+1)
				return
			}
		}
	}); err != nil {
		return Passkey{}, fmt.Errorf("save settings: %w", err)
	}
	if !found {
		return Passkey{}, fmt.Errorf("no passkey found with id %q", id)
	}
	return removed, nil
}

func loginEnrol(store SettingsStore) {
	plaintext, expires, err := mintLoginBootstrap(store)
	if err != nil {
		exitError("%v", err)
	}

	fmt.Printf("login code: %s\n", plaintext)
	fmt.Printf("  expires:   %s (valid for %s, single use)\n", expires, bootstrapCodeTTL)
	fmt.Println("  this code registers a passkey — it is NOT a password and is never accepted in place of one")
	// The host is rewritten to localhost rather than printed as bound: an RP
	// ID must be a domain, so a passkey registered at http://127.0.0.1:PORT
	// cannot exist at all (ADR-016 decision 1).
	if _, port, err := net.SplitHostPort(os.Getenv(EnvAPIListen)); err == nil && port != "" {
		fmt.Printf("  open http://%s:%s/relay/login and enter it to register a passkey\n", webauthnRPID, port)
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

	removed, err := revokePasskey(store, *id)
	if err != nil {
		exitError("%v", err)
	}

	fmt.Printf("revoked passkey %q\n", removed.Name)
	fmt.Printf("  id: %s\n", abbreviatePasskeyID(removed.ID))
	fmt.Println("  it can no longer complete a login assertion; nothing else was touched")
}

// abbreviatePasskeyID keeps enough of a credential id to tell one listed
// passkey from another without putting a value long enough to be mistaken
// for a secret on a shared screen. `revoke --id` still takes the full id,
// not this shortened form.
func abbreviatePasskeyID(id string) string {
	const keep = 12
	if len(id) <= keep+1 {
		return id
	}
	return id[:keep] + "…"
}
