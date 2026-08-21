package main

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
)

// ---------------------------------------------------------------------------
// Remote Clients tab IPC handlers — enrolments (ADR-010 decision 8: "enrolments
// are listed in the Settings UI beside the grants they reach — a credential you
// cannot see is one you will not revoke") plus the read/write surface for the
// `remote` listener block (decision 9).
//
// This file is the IPC sibling of enrol_cmd.go and deliberately shares its
// orchestration: createEnrolment / revokeEnrolment do the work, and the errors
// the UI shows are the errors those functions produce, verbatim. Nothing here
// re-derives "which grants are legal" or "what revocation means" — a second
// copy of either is exactly how the CLI and the tab would drift into
// disagreeing about who can reach what.
// ---------------------------------------------------------------------------

const (
	MsgCreateEnrolment    = "create_enrolment"
	MsgRevokeEnrolment    = "revoke_enrolment"
	MsgUpdateRemoteConfig = "update_remote_config"
)

// ipcCreateEnrolmentMsg is the tab's create form. It mirrors enrolmentRequest
// (the transport-agnostic body the CLI also fills) rather than inventing a
// second shape — enrolment.go's comment on that type anticipated exactly this
// caller.
type ipcCreateEnrolmentMsg struct {
	ClientID   string          `json:"client_id"`
	ProjectIDs []string        `json:"project_ids"`
	Budget     EnrolmentBudget `json:"budget"`
}

// ipcEnrolmentIDMsg names one enrolment. Separate from ipcIDMsg because an
// enrolment is keyed by client id, not by an "id" field, and reusing the
// generic message would make the two look interchangeable when they are not.
type ipcEnrolmentIDMsg struct {
	ClientID string `json:"client_id"`
}

// ipcRemoteConfigMsg edits the `remote` block. Remove is its own field rather
// than "enabled: false with an empty listen" because deleting the block and
// disabling the listener are genuinely different states — an absent block is
// the only one that says nothing was ever configured — and the UI renders them
// differently.
type ipcRemoteConfigMsg struct {
	Remove  bool   `json:"remove"`
	Enabled bool   `json:"enabled"`
	Listen  string `json:"listen"`
}

// enrolmentBundleView is everything the UI is told about an emitted bundle:
// the DIRECTORY, and nothing else.
//
// createEnrolment writes a client private key into that directory. The key
// does not cross this boundary — not its bytes, not a preview, not a "reveal"
// button — because the settings WebView is a rendering surface, and key
// material that reaches it has been copied somewhere nobody will think to
// wipe. The private key travelling to the client once is already the weakest
// step in ADR-010 (decision 8); adding a second copy of it to a browser
// context is not a trade this UI gets to make on the operator's behalf.
//
// The operator is told where the directory is and to MOVE it, which is exactly
// what `relay enrol create` says. The three filenames inside it are static UI
// copy rather than fields here, so there is no field on this struct that a
// future edit could quietly widen from "path" to "contents".
type enrolmentBundleView struct {
	Dir string `json:"dir"`
}

// remoteConfigView is the `remote` block as the tab needs to read it: the raw
// stored values, what they resolve to, and whether auditing is on.
//
// Configured is separate from Enabled because an ABSENT block and a present
// one that resolves to disabled are different facts about the install, and
// decision 9 makes the difference load-bearing: absent means no listener was
// ever asked for, while present-and-off means someone configured one and it is
// not running. Effective carries resolve()'s answer so the UI can show the
// loopback default without hardcoding it a second time.
type remoteConfigView struct {
	// Configured reports whether settings.json has a "remote" block at all.
	Configured bool `json:"configured"`
	// Enabled is the resolved answer — false for an absent block, and false
	// for a block that omits `enabled`. A network listener is never opened by
	// omission (decision 9), which is the opposite default to Audit.
	Enabled bool `json:"enabled"`
	// Listen is the address exactly as stored; empty means "unset, take the
	// default", which is what Effective then shows.
	Listen    string `json:"listen"`
	Effective string `json:"effective"`
	// AuditEnabled gates everything else: the listener refuses to start while
	// auditing is off, so a remote block that looks configured is dead in that
	// state and the UI has to say so rather than let it look live.
	AuditEnabled bool `json:"audit_enabled"`
}

// remoteConfigViewOf projects the `remote` block for the UI.
//
// auditEnabled is passed in rather than derived here because the two callers
// legitimately know different things. The IPC handlers hold the live
// *AuditRecorder and pass its Enabled(), which also accounts for a recorder
// that failed to open its file; the first paint (renderSettingsHTML) has only
// the settings and passes the configured value. The recorder is the more
// truthful of the two and wins wherever it exists.
func remoteConfigViewOf(s *Settings, auditEnabled bool) remoteConfigView {
	// resolve() is nil-safe and is the same function the listener consults, so
	// the tab cannot disagree with NewRemoteServer about what a block means.
	resolved := s.Remote.resolve()
	v := remoteConfigView{
		Configured:   s.Remote != nil,
		Enabled:      resolved.Enabled,
		Effective:    resolved.Listen,
		AuditEnabled: auditEnabled,
	}
	if s.Remote != nil {
		v.Listen = s.Remote.Listen
	}
	return v
}

// enrolmentBudgetDefaults is the conservative starting budget, shipped to the
// UI so the create form's placeholders name the real numbers instead of a
// second copy of them that can rot.
func enrolmentBudgetDefaults() EnrolmentBudget {
	return normalizeEnrolmentBudget(EnrolmentBudget{})
}

// ipcCreateEnrolment signs a client certificate, persists the record, and emits
// the bundle DIRECTORY (never its contents — see enrolmentBundleView).
func ipcCreateEnrolment(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcCreateEnrolmentMsg](raw, MsgCreateEnrolment)
	if !ok {
		return
	}
	clientID := strings.TrimSpace(msg.ClientID)
	if clientID == "" {
		// Matches `relay enrol create`'s "--client-id is required".
		ctx.UI.EmitEvent("onEnrolmentError", "client id is required")
		return
	}
	// Grant legality is deliberately NOT pre-checked here. ValidateEnrolment
	// runs inside createEnrolment's store.With, which is the only place two
	// concurrent creates cannot both claim a client id; a second opinion out
	// here could only ever disagree with the one that counts. The UI offering
	// nothing but remote projects is a courtesy, not the enforcement.
	bundle, err := createEnrolment(ctx.Store, enrolmentRequest{
		ClientID:   clientID,
		ProjectIDs: msg.ProjectIDs,
		Budget:     msg.Budget,
	})
	if err != nil {
		ctx.UI.EmitEvent("onEnrolmentError", err.Error())
		return
	}
	ctx.UI.EmitEvent("onEnrolmentCreated",
		marshalForUI(bundle.Enrolment),
		marshalForUI(enrolmentBundleView{Dir: bundle.Dir}))
}

// ipcRevokeEnrolment deletes an enrolment through revokeEnrolment, which is
// what makes revocation mean anything: it fires the revocation hook so the
// listener severs LIVE connections holding that certificate, and removes the
// emitted bundle. Deleting the record directly (RemoveEnrolment) would leave a
// compromised agent in its scanner loop working indefinitely, since it never
// needs to reconnect.
func ipcRevokeEnrolment(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcEnrolmentIDMsg](raw, MsgRevokeEnrolment)
	if !ok || msg.ClientID == "" {
		return
	}
	removed, err := revokeEnrolment(ctx.Store, msg.ClientID)
	if err != nil {
		ctx.UI.EmitEvent("onEnrolmentError", err.Error())
		return
	}
	// The fingerprint rides along for the same reason `relay enrol revoke`
	// prints it: once the record is gone it is the only thing that identifies
	// that client's calls in the audit log.
	ctx.UI.EmitEvent("onEnrolmentRevoked", removed.ClientID, removed.Fingerprint)
}

// ipcUpdateRemoteConfig writes the `remote` block.
func ipcUpdateRemoteConfig(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcRemoteConfigMsg](raw, MsgUpdateRemoteConfig)
	if !ok {
		return
	}
	listen := strings.TrimSpace(msg.Listen)
	if !msg.Remove && listen != "" {
		if err := validateRemoteListen(listen); err != nil {
			ctx.UI.EmitEvent("onRemoteConfigError", err.Error())
			return
		}
	}

	if !ctx.withSettings(func(s *Settings) {
		if msg.Remove {
			// Back to "no block at all", which is a state the operator can
			// otherwise never return to once they have touched this form.
			s.Remote = nil
			return
		}
		if s.Remote == nil {
			s.Remote = &RemoteConfig{}
		}
		// Empty stays empty rather than being filled with the default: the
		// listener applies resolve()'s loopback default itself, and writing it
		// out here would freeze today's default into every settings.json.
		s.Remote.Listen = listen
		if msg.Enabled {
			enabled := true
			s.Remote.Enabled = &enabled
		} else {
			// Off collapses to an ABSENT key rather than an explicit false.
			// Both resolve to disabled, so storing one spelling keeps every
			// disabled block identical — the same reasoning normalizeProjectKind
			// applies to a local project's Kind. It also means creating the
			// block by setting only an address can never write `enabled: true`
			// on the operator's behalf: opening a network listener is a thing
			// they say, not a thing relay infers.
			s.Remote.Enabled = nil
		}
	}) {
		return
	}

	ctx.UI.EmitEvent("onRemoteConfigUpdated",
		marshalForUI(remoteConfigViewOf(ctx.Store.Get(), ctx.Audit.Enabled())))
}

// validateRemoteListen refuses an address the listener could only fail to bind,
// at the moment the operator is still looking at the field. Without it a typo
// persists silently and surfaces as a listener that is simply absent after the
// next restart, with the reason in a log nobody is reading.
//
// An empty HOST (":9910") is NOT refused: binding every interface is a
// deliberate, documented act (decision 9 defaults to loopback so that reaching
// relay from a VM has to be chosen, not so that choosing it is forbidden). The
// UI warns about it instead.
func validateRemoteListen(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen address %q is not host:port (e.g. %s)", addr, defaultRemoteListen)
	}
	if port == "" {
		return fmt.Errorf("listen address %q has no port (e.g. %s)", addr, defaultRemoteListen)
	}
	// LookupPort resolves numeric ports without touching the network and also
	// accepts the service names tls.Listen would accept, so this agrees with
	// what the listener will actually do.
	if _, err := net.LookupPort("tcp", port); err != nil {
		return fmt.Errorf("listen address %q has an invalid port %q", addr, port)
	}
	return nil
}
