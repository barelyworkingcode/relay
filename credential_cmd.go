package main

import (
	"errors"
	"flag"
	"fmt"
	"slices"
	"strings"
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
		{"list", func(_ []string) { credentialList(store) }},
		{"revoke", func(a []string) { credentialRevoke(store, a) }},
	}, args)
}

// capabilityClasses is the whole vocabulary. A class string outside it is
// refused rather than stored: Grants compares against these constants, so a
// typo'd class would leave the credential inert with nothing on any operator
// surface to show why.
var capabilityClasses = []CapabilityClass{ClassRead, ClassConfigure, ClassGrant, ClassExecute}

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

	var cred APICredential
	var plaintext string
	var mintErr error
	if err := store.With(func(s *Settings) {
		cred, plaintext, mintErr = s.Mint(name, classes)
	}); err != nil {
		return APICredential{}, "", fmt.Errorf("save settings: %w", err)
	}
	if mintErr != nil {
		return APICredential{}, "", mintErr
	}
	return cred, plaintext, nil
}

func revokeAPICredential(store SettingsStore, id string) (APICredential, error) {
	if strings.TrimSpace(id) == "" {
		return APICredential{}, errors.New("a credential id is required")
	}

	var removed APICredential
	var found, reserved bool
	// Resolve and remove inside one store.With, matching resolveAndRemove:
	// a separate Get() then With() is a TOCTOU window on a file two
	// processes write.
	if err := store.With(func(s *Settings) {
		cred := s.FindAPICredential(id)
		if cred == nil {
			return
		}
		found = true
		if cred.Name == legacyFrontendCredentialName {
			reserved = true
			return
		}
		removed, _ = s.RemoveAPICredential(id)
	}); err != nil {
		return APICredential{}, fmt.Errorf("save settings: %w", err)
	}
	if !found {
		return APICredential{}, fmt.Errorf("no credential found with id %q", id)
	}
	if reserved {
		return APICredential{}, errReservedCredentialName
	}
	return removed, nil
}

func credentialMint(store SettingsStore, args []string) {
	fs := flag.NewFlagSet("credential mint", flag.ExitOnError)
	name := fs.String("name", "", "human-readable name for this credential (required)")
	var classes stringSlice
	fs.Var(&classes, "class", "capability class this credential may exercise (repeatable): read, configure, grant, execute")
	fs.Parse(args)

	cred, plaintext, err := mintAPICredential(store, credentialMintRequest{Name: *name, Classes: []string(classes)})
	if err != nil {
		exitError("%v", err)
	}

	fmt.Printf("minted credential %q\n", cred.Name)
	fmt.Printf("  id:      %s\n", cred.ID)
	fmt.Printf("  classes: %s\n", formatClasses(cred.Classes))
	fmt.Printf("  created: %s\n", cred.Created)
	fmt.Printf("  token:   %s\n", plaintext)
	fmt.Println("  this token is shown ONCE and is not recoverable — only its SHA-256 is stored")
	fmt.Println("  present it as: Authorization: Bearer <token>")
}

func credentialList(store SettingsStore) {
	s := store.Get()

	if len(s.APICredentials) == 0 {
		fmt.Println("no credentials")
		return
	}

	w := newTabWriter()
	fmt.Fprintln(w, "ID\tNAME\tCLASSES\tCREATED")
	for _, c := range s.APICredentials {
		// Neither the hash nor the plaintext is printed. The hash is a
		// verifier for a live secret, so a listing that showed it would put
		// an offline-guessable value on any screen or scrollback that ran
		// this command.
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.ID, c.Name, formatClasses(c.Classes), c.Created)
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
