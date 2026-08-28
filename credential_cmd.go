package main

import (
	"errors"
	"flag"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Control-plane credentials are minted on the host, by the user who owns the
// config dir; nothing in this file is reachable over a socket. It runs in a
// separate process from the tray, so every read and write goes through the
// store rather than a cached settings view — Authorize reads through
// freshSettings, which is what makes a credential minted here authenticate on
// its very next request.
func runCredentialCommand(args []string) {
	store := NewSettingsStore()
	runSubcommands("credential", []cliSubcommand{
		{"mint", func(a []string) { credentialMint(store, a) }},
		{"list", func(a []string) { credentialList(store, a) }},
		{"revoke", func(a []string) { credentialRevoke(store, a) }},
	}, args)
}

// capabilityClasses is the whole vocabulary. A class string outside it is
// refused rather than stored: Grants compares against these constants, so a
// typo'd class would leave the credential inert with nothing on any operator
// surface to show why.
var capabilityClasses = []CapabilityClass{ClassRead, ClassConfigure, ClassGrant, ClassExecute, ClassProxy}

var errReservedCredentialName = fmt.Errorf("%q is reserved for the RELAY_FRONTEND_TOKEN migration, which rewrites its hash on every relay start; it cannot be minted or revoked by hand", legacyFrontendCredentialName)

func formatClasses[T ~string](classes []T) string {
	if len(classes) == 0 {
		return "-"
	}
	parts := make([]string, len(classes))
	for i, c := range classes {
		parts[i] = string(c)
	}
	return strings.Join(parts, ",")
}

func parseCapabilityClasses(raw []string) ([]CapabilityClass, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("at least one class is required (%s); a credential with no class can reach nothing", formatClasses(capabilityClasses))
	}
	out := make([]CapabilityClass, 0, len(raw))
	for _, r := range raw {
		c := CapabilityClass(strings.TrimSpace(r))
		if !slices.Contains(capabilityClasses, c) {
			return nil, fmt.Errorf("unknown class %q; valid classes are %s", r, formatClasses(capabilityClasses))
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out, nil
}

type credentialMintRequest struct {
	Name    string
	Classes []string
	// TTL zero means the credential never expires, matching
	// Settings.MintFor. A negative value is an operator mistake and is
	// refused rather than silently read as "never".
	TTL time.Duration
}

// mintAPICredential returns the PLAINTEXT token alongside the record. It is
// the only moment that value exists; nothing stores it and no later call can
// reconstruct it from the record.
func mintAPICredential(store SettingsStore, req credentialMintRequest) (APICredential, string, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return APICredential{}, "", errors.New("a credential name is required")
	}
	if name == legacyFrontendCredentialName {
		return APICredential{}, "", errReservedCredentialName
	}
	classes, err := parseCapabilityClasses(req.Classes)
	if err != nil {
		return APICredential{}, "", err
	}
	if req.TTL < 0 {
		return APICredential{}, "", fmt.Errorf("a negative lifetime (%s) is not a credential; omit --ttl for one that never expires", req.TTL)
	}

	var cred APICredential
	var plaintext string
	var mintErr error
	// Reaped in the same store.With as the mint, which is the whole of the
	// reaping schedule: this is a write that was happening anyway, so
	// sweeping here costs nothing and needs no timer.
	if err := store.With(func(s *Settings) {
		reapExpiredAPICredentials(s)
		cred, plaintext, mintErr = s.MintFor(name, classes, req.TTL)
	}); err != nil {
		return APICredential{}, "", fmt.Errorf("save settings: %w", err)
	}
	if mintErr != nil {
		return APICredential{}, "", mintErr
	}
	return cred, plaintext, nil
}

func revokeAPICredential(store SettingsStore, id string) (APICredential, error) {
	return revokeAPICredentialIf(store, id, nil)
}

// revokeAPICredentialIf revokes by id, refusing whatever `permitted` rejects
// on top of the reserved-name refusal every caller gets.
//
// The extra gate runs inside this store.With rather than as a lookup in the
// caller for the reason the resolve and the remove already share one: a
// separate Get() then With() is a TOCTOU window on a file two processes
// write, and a gate on the far side of that window is a gate that can be
// stepped around.
func revokeAPICredentialIf(store SettingsStore, id string, permitted func(APICredential) error) (APICredential, error) {
	if strings.TrimSpace(id) == "" {
		return APICredential{}, errors.New("a credential id is required")
	}

	var removed APICredential
	var found bool
	var refusal error
	if err := store.With(func(s *Settings) {
		cred := s.FindAPICredential(id)
		if cred == nil {
			return
		}
		found = true
		if cred.Name == legacyFrontendCredentialName {
			refusal = errReservedCredentialName
			return
		}
		if permitted != nil {
			if err := permitted(*cred); err != nil {
				refusal = err
				return
			}
		}
		removed, _ = s.RemoveAPICredential(id)
	}); err != nil {
		return APICredential{}, fmt.Errorf("save settings: %w", err)
	}
	if !found {
		return APICredential{}, fmt.Errorf("no credential found with id %q", id)
	}
	if refusal != nil {
		return APICredential{}, refusal
	}
	return removed, nil
}

func credentialMint(store SettingsStore, args []string) {
	fs := flag.NewFlagSet("credential mint", flag.ExitOnError)
	name := fs.String("name", "", "human-readable name for this credential (required)")
	ttl := fs.Duration("ttl", 0, "how long this credential lives (e.g. 12h); omit for one that never expires")
	var classes stringSlice
	fs.Var(&classes, "class", "capability class this credential may exercise (repeatable): "+formatClasses(capabilityClasses))
	fs.Parse(args)

	cred, plaintext, err := mintAPICredential(store, credentialMintRequest{Name: *name, Classes: []string(classes), TTL: *ttl})
	if err != nil {
		exitError("%v", err)
	}

	fmt.Printf("minted credential %q\n", cred.Name)
	fmt.Printf("  id:      %s\n", cred.ID)
	fmt.Printf("  classes: %s\n", formatClasses(cred.Classes))
	fmt.Printf("  created: %s\n", cred.Created)
	fmt.Printf("  expires: %s\n", formatCredentialExpiry(cred, time.Now()))
	fmt.Printf("  token:   %s\n", plaintext)
	fmt.Println("  this token is shown ONCE and is not recoverable — only its SHA-256 is stored")
	fmt.Println("  present it as: Authorization: Bearer <token>")
}

// formatCredentialExpiry renders the EXPIRES column. "never" is spelled out
// rather than left blank: a blank cell reads as missing data, and this one
// is a decision. An expired record is marked, which is the only reason
// --include-expired is worth having — an operator wants to see what the next
// mint is about to sweep.
func formatCredentialExpiry(c APICredential, now time.Time) string {
	if c.Expires == "" {
		return "never"
	}
	if c.Expired(now) {
		return c.Expires + " (expired)"
	}
	return c.Expires
}

func credentialList(store SettingsStore, args []string) {
	fs := flag.NewFlagSet("credential list", flag.ExitOnError)
	includeExpired := fs.Bool("include-expired", false, "also list credentials that have expired and are awaiting the next mint's reap")
	fs.Parse(args)

	s := store.Get()
	now := time.Now()

	shown := make([]APICredential, 0, len(s.APICredentials))
	for _, c := range s.APICredentials {
		if *includeExpired || !c.Expired(now) {
			shown = append(shown, c)
		}
	}

	if len(shown) == 0 {
		if !*includeExpired && len(s.APICredentials) > 0 {
			fmt.Println("no live credentials (--include-expired shows the expired ones)")
			return
		}
		fmt.Println("no credentials")
		return
	}

	w := newTabWriter()
	fmt.Fprintln(w, "ID\tNAME\tCLASSES\tCREATED\tEXPIRES")
	for _, c := range shown {
		// Neither the hash nor the plaintext is printed. The hash is a
		// verifier for a live secret, so a listing that showed it would put
		// an offline-guessable value on any screen or scrollback that ran
		// this command.
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", c.ID, c.Name, formatClasses(c.Classes), c.Created, formatCredentialExpiry(c, now))
	}
	w.Flush()
}

func credentialRevoke(store SettingsStore, args []string) {
	fs := flag.NewFlagSet("credential revoke", flag.ExitOnError)
	id := fs.String("id", "", "id of the credential to revoke (required)")
	fs.Parse(args)

	removed, err := revokeAPICredential(store, *id)
	if err != nil {
		exitError("%v", err)
	}

	fmt.Printf("revoked credential %q\n", removed.ID)
	fmt.Printf("  name:    %s\n", removed.Name)
	fmt.Printf("  classes: %s\n", formatClasses(removed.Classes))
	fmt.Println("  its token stops authenticating on the next request; nothing else was touched")
}
