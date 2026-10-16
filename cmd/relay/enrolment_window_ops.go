package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/barelyworkingcode/relay/internal/audit"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/enrolment"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/presence"
)

// enrolmentWindowTTL is how long an access-profile window stays open when
// nothing lodges: long enough to walk to the remote machine and run its
// register command, short enough that an open port does not linger forgotten.
const enrolmentWindowTTL = 5 * time.Minute

// enrolmentWindowTargetProfile is the target of a window that admits one
// remote machine into one access profile.
const enrolmentWindowTargetProfile = "profile"

// auditCredentialEnrolmentWindow is the credential kind of an approved open:
// the window is what lets a stranger's request reach the table, so its opening
// is recorded as an issuance, before the port binds.
const auditCredentialEnrolmentWindow = "enrolment_window"

// enrolmentWindowView is one open window, or {"open": false}.
type enrolmentWindowView struct {
	Open        bool   `json:"open"`
	Target      string `json:"target,omitempty"`
	ProfileID   string `json:"profile_id,omitempty"`
	ProfileName string `json:"profile_name,omitempty"`
	Expires     string `json:"expires,omitempty"`
	Listen      string `json:"listen,omitempty"`
}

// EnrolmentWindowOps opens and closes the timed window that lets one remote
// machine ask to enrol into one access profile. Only Open is gated: a close
// narrows what is reachable, and Status is a read.
type EnrolmentWindowOps struct {
	Store config.SettingsStore
	Queue *config.CommandQueue
	Audit *audit.AuditRecorder
	// Gate is the presence check Open demands before it records anything. A
	// nil Gate refuses — see requireGate.
	Gate   *presence.Gate
	Remote *RemoteSupervisor
	// Notify raises the console banner for a window that expired with
	// nothing lodged. Nil raises nothing.
	Notify func(title, body string)
	Clock  serverClock
}

var errEnrolmentWindowOpsUnavailable = errors.New("enrolment windows are not available in this relay process")

func (o *EnrolmentWindowOps) auditor() IssuanceAuditor {
	if o == nil {
		return nil
	}
	return issuanceAuditorOrNil(o.Audit)
}

// Open asks for presence and, once approved, binds the enrolment listener for
// profileID. Every refusal that needs no human comes first: an unknown or
// local profile and a remote listener that is off never raise a prompt.
//
// This is subtle: the issuance row is written between the prompt and the
// bind, and a row that cannot be written means nothing binds.
func (o *EnrolmentWindowOps) Open(ctx context.Context, profileID, via string) (view enrolmentWindowView, err error) {
	ev := logging.BeginEvent(ctx, "enrolment.window.open")
	profileID = strings.TrimSpace(profileID)
	defer func() {
		ev.Set("profile_id", profileID)
		if err == nil {
			ev.Set("expires", view.Expires)
		}
		endEvent(ev, err)
	}()
	if o == nil || o.Remote == nil {
		return enrolmentWindowView{}, errEnrolmentWindowOpsUnavailable
	}
	if profileID == "" {
		return enrolmentWindowView{}, enrolment.Invalid("profile id is required")
	}
	settings := config.FreshSettings(o.Store)
	proj, _ := config.FindProjectByID(settings, profileID)
	if proj == nil || !proj.IsRemote() {
		return enrolmentWindowView{}, enrolment.Invalid(fmt.Sprintf("%q is not an access profile", profileID))
	}
	if !resolveRemoteConfig(settings.Remote).Enabled {
		return enrolmentWindowView{}, enrolment.Invalid("turn the remote listener on first: Settings > Remote Clients, or relay remote set")
	}
	if err := requireIssuanceAuditor(o.auditor()); err != nil {
		return enrolmentWindowView{}, err
	}

	name := profileDisplayName(settings, profileID)
	grant, err := requireGate(o.Gate, ctx, "enrolment.window.open",
		singleStringDigest("enrolment.window.open", "profile_id", profileID),
		fmt.Sprintf("open a five-minute window for one remote machine to request enrolment into the access profile %q", name),
		presenceAttempt{auditor: o.auditor(), via: via, subject: profileID})
	if err != nil {
		return enrolmentWindowView{}, err
	}

	if err := recordIssuance(o.auditor(), audit.CredentialIssuance{
		Credential: auditCredentialEnrolmentWindow,
		Subject:    profileID,
		Grants:     []string{profileID},
		Via:        via,
		PresenceID: grant.ID(),
	}); err != nil {
		return enrolmentWindowView{}, fmt.Errorf("an enrolment window was approved but could not be recorded in the audit log, so none was opened: %w", err)
	}

	o.Remote.OnEnrolmentWindowClosed(o.windowClosed)
	expires := clockNow(o.Clock).Add(enrolmentWindowTTL)
	addr, err := o.Remote.OpenEnrolmentWindow(profileID, expires)
	if err != nil {
		return enrolmentWindowView{}, err
	}
	return enrolmentWindowView{
		Open:        true,
		Target:      enrolmentWindowTargetProfile,
		ProfileID:   profileID,
		ProfileName: name,
		Expires:     expires.UTC().Format(time.RFC3339),
		Listen:      addr,
	}, nil
}

// windowClosed tells the console about the one close nobody asked for: the
// timer running out with nothing lodged.
func (o *EnrolmentWindowOps) windowClosed(reason string) {
	if reason == windowClosedExpired && o.Notify != nil {
		o.Notify("Relay", "Enrolment closed; nothing enrolled.")
	}
}

// Close closes every open window and names what it closed. Ungated: closing
// narrows. Nothing open is not an error.
//
// The close event itself (reason "cancelled") is written by the supervisor, in
// the one place every close goes through.
func (o *EnrolmentWindowOps) Close(_ context.Context, _ string) (closed []string, err error) {
	if o == nil || o.Remote == nil {
		return nil, errEnrolmentWindowOpsUnavailable
	}
	if o.Remote.CloseEnrolmentWindow(windowClosedCancelled) {
		closed = append(closed, enrolmentWindowTargetProfile)
	}
	return closed, nil
}

// Status lists the open windows, one entry each.
func (o *EnrolmentWindowOps) Status() []enrolmentWindowView {
	if o == nil || o.Remote == nil {
		return nil
	}
	var out []enrolmentWindowView
	if v := windowViewOf(o.Remote, config.DisplaySettings(o.Store)); v.Open {
		out = append(out, v)
	}
	return out
}

// windowViewOf projects the supervisor's open window; {"open": false} with none.
func windowViewOf(sup *RemoteSupervisor, s *config.Settings) enrolmentWindowView {
	profileID, expires, addr, open := sup.EnrolmentWindow()
	if !open {
		return enrolmentWindowView{}
	}
	return enrolmentWindowView{
		Open:        true,
		Target:      enrolmentWindowTargetProfile,
		ProfileID:   profileID,
		ProfileName: profileDisplayName(s, profileID),
		Expires:     expires.UTC().Format(time.RFC3339),
		Listen:      addr,
	}
}

// currentEnrolmentWindowView is windowViewOf for the running server; closed in
// a process that runs none.
func currentEnrolmentWindowView(s *config.Settings) enrolmentWindowView {
	if appInstance == nil {
		return enrolmentWindowView{}
	}
	appInstance.addrMu.RLock()
	sup := appInstance.remote
	appInstance.addrMu.RUnlock()
	return windowViewOf(sup, s)
}

// profileDisplayName is the name the operator reads for an access profile,
// falling back to its id when it has no name or is gone.
func profileDisplayName(s *config.Settings, id string) string {
	if s != nil {
		if proj, _ := config.FindProjectByID(s, id); proj != nil && strings.TrimSpace(proj.Name) != "" {
			return proj.Name
		}
	}
	return id
}
