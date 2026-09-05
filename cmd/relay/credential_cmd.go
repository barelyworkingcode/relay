package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
)

// Control-plane credentials are minted on the host, by the user who owns the
// config dir. Mint and revoke are brokered (ADR-017 decision 2): this
// process holds no sealer and cannot write settings.json itself (§5.4), so
// it dials the running tray over admin_op and lets CredentialOps — the same
// core the gate lives in — do the work. `list` is unaffected: it reads
// settings.json directly and keeps working with the tray stopped.
func runCredentialCommand(args []string) {
	store := config.NewSettingsStore()
	runSubcommands("credential", []cliSubcommand{
		{"mint", credentialMint},
		{"list", func(a []string) { credentialList(store, a) }},
		{"revoke", credentialRevoke},
	}, args)
}

// capabilityClasses is the whole vocabulary. A class string outside it is
// refused rather than stored: Grants compares against these constants, so a
// typo'd class would leave the credential inert with nothing on any operator
// surface to show why.
var capabilityClasses = []control.CapabilityClass{control.ClassRead, control.ClassConfigure, control.ClassGrant, control.ClassExecute, control.ClassProxy}

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

func parseCapabilityClasses(raw []string) ([]control.CapabilityClass, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("at least one class is required (%s); a credential with no class can reach nothing", formatClasses(capabilityClasses))
	}
	out := make([]control.CapabilityClass, 0, len(raw))
	for _, r := range raw {
		c := control.CapabilityClass(strings.TrimSpace(r))
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
	Name    string   `json:"name"`
	Classes []string `json:"classes"`
	// TTL zero means the credential never expires, matching
	// mintAPICredentialFor. A negative value is an operator mistake and is
	// refused rather than silently read as "never".
	TTL time.Duration `json:"ttl"`
}

// credentialMint parses the flags, dials the service, and prints what
// CredentialOps.Mint hands back. Validation (name required, class vocabulary,
// non-negative TTL) lives in the core now, not here: the request travels to
// the tray unchecked and Mint is what refuses it, the same as every other
// brokered command.
func credentialMint(args []string) {
	fs := flag.NewFlagSet("credential mint", flag.ExitOnError)
	name := fs.String("name", "", "human-readable name for this credential (required)")
	ttl := fs.Duration("ttl", 0, "how long this credential lives (e.g. 12h); omit for one that never expires")
	var classes stringSlice
	fs.Var(&classes, "class", "capability class this credential may exercise (repeatable): "+formatClasses(capabilityClasses))
	fs.Parse(args)

	client := requireService("relay credential mint")
	req, err := json.Marshal(credentialMintRequest{Name: *name, Classes: []string(classes), TTL: *ttl})
	if err != nil {
		exitError("%v", err)
	}
	raw, err := client.AdminOp("credential.mint", req)
	if err != nil {
		exitError("%s", adminOpErrorText(err))
	}
	var result credentialMintResult
	if err := json.Unmarshal(raw, &result); err != nil {
		exitError("parse response: %v", err)
	}

	fmt.Printf("minted credential %q\n", result.Credential.Name)
	fmt.Printf("  id:      %s\n", result.Credential.ID)
	fmt.Printf("  classes: %s\n", formatClasses(result.Credential.Classes))
	fmt.Printf("  created: %s\n", result.Credential.Created)
	fmt.Printf("  expires: %s\n", formatCredentialExpiry(result.Credential, time.Now()))
	fmt.Printf("  token:   %s\n", result.Token)
	fmt.Println("  this token is shown ONCE and is not recoverable — only its SHA-256 is stored")
	fmt.Println("  present it as: Authorization: Bearer <token>")
}

// formatCredentialExpiry renders the EXPIRES column. "never" is spelled out
// rather than left blank: a blank cell reads as missing data, and this one
// is a decision. An expired record is marked, which is the only reason
// --include-expired is worth having — an operator wants to see what the next
// mint is about to sweep.
func formatCredentialExpiry(c config.APICredential, now time.Time) string {
	if c.Expires == "" {
		return "never"
	}
	if c.Expired(now) {
		return c.Expires + " (expired)"
	}
	return c.Expires
}

func credentialList(store config.SettingsStore, args []string) {
	fs := flag.NewFlagSet("credential list", flag.ExitOnError)
	includeExpired := fs.Bool("include-expired", false, "also list credentials that have expired and are awaiting the next mint's reap")
	fs.Parse(args)

	s := store.Get()
	now := time.Now()

	shown := make([]config.APICredential, 0, len(s.APICredentials))
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

func credentialRevoke(args []string) {
	fs := flag.NewFlagSet("credential revoke", flag.ExitOnError)
	id := fs.String("id", "", "id of the credential to revoke (required)")
	fs.Parse(args)

	client := requireService("relay credential revoke")
	req, err := json.Marshal(credentialRevokeRequest{ID: *id})
	if err != nil {
		exitError("%v", err)
	}
	raw, err := client.AdminOp("credential.revoke", req)
	if err != nil {
		exitError("%s", adminOpErrorText(err))
	}
	var removed config.APICredential
	if err := json.Unmarshal(raw, &removed); err != nil {
		exitError("parse response: %v", err)
	}

	fmt.Printf("revoked credential %q\n", removed.ID)
	fmt.Printf("  name:    %s\n", removed.Name)
	fmt.Printf("  classes: %s\n", formatClasses(removed.Classes))
	fmt.Println("  its token stops authenticating on the next request; nothing else was touched")
}
