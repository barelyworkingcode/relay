package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/service"
)

// The read ops below are how every CLI list command reaches configuration:
// the running tray is the only reader of settings.json for a normal CLI
// command. Each op answers with a purpose-specific projection of a fresh
// snapshot, never a Settings value, so sealed fields, token hashes and
// environment values have no field to travel in. None is presence-gated
// (they change nothing) and none enters the config queue lane: one snapshot
// is a consistent read.

func adminReadSnapshot(r *appRouter) (*config.Settings, error) {
	if r.store == nil {
		return nil, errors.New("settings are not available in this relay process")
	}
	return config.FreshSettings(r.store), nil
}

// credentialListItem carries no hash: it verifies a live secret, so a
// listing that showed it would put an offline-guessable value on the wire.
type credentialListItem struct {
	ID      string                    `json:"id"`
	Name    string                    `json:"name,omitempty"`
	Classes []control.CapabilityClass `json:"classes"`
	Created string                    `json:"created,omitempty"`
	Expires string                    `json:"expires,omitempty"`
}

type credentialListResult struct {
	Credentials []credentialListItem `json:"credentials"`
}

// credential returns the record shape formatCredentialExpiry reads, still
// without a hash.
func (i credentialListItem) credential() config.APICredential {
	return config.APICredential{ID: i.ID, Name: i.Name, Classes: i.Classes, Created: i.Created, Expires: i.Expires}
}

func adminCredentialList(ctx context.Context, r *appRouter, _ json.RawMessage) (_ json.RawMessage, err error) {
	ev := logging.BeginEvent(ctx, "credential.list")
	defer func() { endEvent(ev, err) }()
	s, err := adminReadSnapshot(r)
	if err != nil {
		return nil, err
	}
	items := make([]credentialListItem, 0, len(s.APICredentials))
	for _, c := range s.APICredentials {
		items = append(items, credentialListItem{ID: c.ID, Name: c.Name, Classes: c.Classes, Created: c.Created, Expires: c.Expires})
	}
	ev.Set("count", len(items))
	return marshalAdminResult(credentialListResult{Credentials: items})
}

// serviceListItem omits Env and WorkingDir: environment values are where a
// service's secrets live, and a listing has no use for either.
type serviceListItem struct {
	ID           string                     `json:"id"`
	DisplayName  string                     `json:"display_name"`
	Command      string                     `json:"command"`
	Args         []string                   `json:"args,omitempty"`
	URL          string                     `json:"url,omitempty"`
	Autostart    bool                       `json:"autostart"`
	Capabilities []config.ServiceCapability `json:"capabilities"`
}

// serviceListResult carries the restart-supervision state, which exists
// only in the tray's memory, beside the records it decorates.
type serviceListResult struct {
	Services []serviceListItem                    `json:"services"`
	Statuses map[string]service.SupervisionStatus `json:"statuses"`
}

func adminServiceList(ctx context.Context, r *appRouter, _ json.RawMessage) (_ json.RawMessage, err error) {
	ev := logging.BeginEvent(ctx, "service.list")
	defer func() { endEvent(ev, err) }()
	ops, err := requireServiceOps(r)
	if err != nil {
		return nil, err
	}
	s, err := adminReadSnapshot(r)
	if err != nil {
		return nil, err
	}
	items := make([]serviceListItem, 0, len(s.Services))
	for _, svc := range s.Services {
		items = append(items, serviceListItem{
			ID: svc.ID, DisplayName: svc.DisplayName, Command: svc.Command, Args: svc.Args,
			URL: svc.URL, Autostart: svc.Autostart, Capabilities: svc.Capabilities,
		})
	}
	statuses := ops.Registry.SupervisionStatuses()
	if statuses == nil {
		statuses = map[string]service.SupervisionStatus{}
	}
	ev.Set("count", len(items))
	return marshalAdminResult(serviceListResult{Services: items, Statuses: statuses})
}

// mcpListItem omits Env and OAuthState for the same reason serviceListItem
// omits Env.
type mcpListItem struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name"`
	Transport   string   `json:"transport,omitempty"`
	Command     string   `json:"command,omitempty"`
	Args        []string `json:"args,omitempty"`
	URL         string   `json:"url,omitempty"`
	HTTP        bool     `json:"http"`
}

type mcpListResult struct {
	Mcps []mcpListItem `json:"mcps"`
}

func adminMcpList(ctx context.Context, r *appRouter, _ json.RawMessage) (_ json.RawMessage, err error) {
	ev := logging.BeginEvent(ctx, "mcp.list")
	defer func() { endEvent(ev, err) }()
	s, err := adminReadSnapshot(r)
	if err != nil {
		return nil, err
	}
	items := make([]mcpListItem, 0, len(s.ExternalMcps))
	for _, m := range s.ExternalMcps {
		items = append(items, mcpListItem{
			ID: m.ID, DisplayName: m.DisplayName, Transport: m.Transport,
			Command: m.Command, Args: m.Args, URL: m.URL, HTTP: m.IsHTTP(),
		})
	}
	ev.Set("count", len(items))
	return marshalAdminResult(mcpListResult{Mcps: items})
}

// loginListResult reuses passkeyView, the projection the Passkeys tab
// already renders: no public-key coordinates.
type loginListResult struct {
	Passkeys []passkeyView `json:"passkeys"`
}

func adminLoginList(ctx context.Context, r *appRouter, _ json.RawMessage) (_ json.RawMessage, err error) {
	ev := logging.BeginEvent(ctx, "login.list")
	defer func() { endEvent(ev, err) }()
	s, err := adminReadSnapshot(r)
	if err != nil {
		return nil, err
	}
	views := passkeyViews(s)
	ev.Set("count", len(views))
	return marshalAdminResult(loginListResult{Passkeys: views})
}

type eveListResult struct {
	Passkeys []evePasskeyView `json:"passkeys"`
}

func adminEveList(ctx context.Context, r *appRouter, _ json.RawMessage) (_ json.RawMessage, err error) {
	ev := logging.BeginEvent(ctx, "eve.list")
	defer func() { endEvent(ev, err) }()
	s, err := adminReadSnapshot(r)
	if err != nil {
		return nil, err
	}
	views := evePasskeyViews(s)
	ev.Set("count", len(views))
	return marshalAdminResult(eveListResult{Passkeys: views})
}

// enrolmentListItem is an enrolment record's public facts: the certificate
// fingerprint is a hash of a public certificate, and no private key is ever
// in settings.
type enrolmentListItem struct {
	ClientID    string                 `json:"client_id"`
	Fingerprint string                 `json:"fingerprint"`
	ProjectIDs  []string               `json:"project_ids"`
	CLIAdmin    bool                   `json:"cli_admin"`
	Budget      config.EnrolmentBudget `json:"budget"`
	CreatedAt   string                 `json:"created_at"`
}

type enrolmentListResult struct {
	Enrolments []enrolmentListItem `json:"enrolments"`
}

func adminEnrolmentList(ctx context.Context, r *appRouter, _ json.RawMessage) (_ json.RawMessage, err error) {
	ev := logging.BeginEvent(ctx, "enrolment.list")
	defer func() { endEvent(ev, err) }()
	s, err := adminReadSnapshot(r)
	if err != nil {
		return nil, err
	}
	items := make([]enrolmentListItem, 0, len(s.Enrolments))
	for _, e := range s.Enrolments {
		items = append(items, enrolmentListItem{
			ClientID: e.ClientID, Fingerprint: e.Fingerprint, ProjectIDs: e.ProjectIDs,
			CLIAdmin: e.CLIAdmin, Budget: e.Budget, CreatedAt: e.CreatedAt,
		})
	}
	ev.Set("count", len(items))
	return marshalAdminResult(enrolmentListResult{Enrolments: items})
}

type grantViewRequest struct {
	Project string `json:"project,omitempty"`
}

type grantViewResult struct {
	Grants []grantView `json:"grants"`
}

// adminGrantView answers with an empty list, not an error, for a selector
// that matches nothing: `relay grant` owns the wording of that refusal.
func adminGrantView(ctx context.Context, r *appRouter, args json.RawMessage) (_ json.RawMessage, err error) {
	ev := logging.BeginEvent(ctx, "grant.view")
	defer func() { endEvent(ev, asInvalid(err)) }()
	req := grantViewRequest{}
	if len(args) > 0 {
		if req, err = decodeAdminArgs[grantViewRequest]("grant.view", args); err != nil {
			return nil, err
		}
	}
	s, err := adminReadSnapshot(r)
	if err != nil {
		return nil, err
	}
	records := selectGrantRecords(s.Projects, req.Project)
	views := make([]grantView, 0, len(records))
	for _, p := range records {
		views = append(views, newGrantView(s, p))
	}
	ev.Set("count", len(views))
	return marshalAdminResult(grantViewResult{Grants: views})
}

// resolveServiceRef and resolveMcpRef turn an --id or --name into the
// record's id against a fresh snapshot, inside the op that acts on it, so
// the resolution and the mutation see the same tray state.
func resolveServiceRef(r *appRouter, id, name string) (string, error) {
	s, err := adminReadSnapshot(r)
	if err != nil {
		return "", err
	}
	if resolved := s.ResolveServiceID(id, name); resolved != "" {
		return resolved, nil
	}
	if id != "" {
		return "", notFoundf("no service found with id %q", id)
	}
	return "", notFoundf("no service found with name %q", name)
}

func resolveMcpRef(r *appRouter, id, name string) (string, error) {
	s, err := adminReadSnapshot(r)
	if err != nil {
		return "", err
	}
	if resolved := s.ResolveMcpID(id, name); resolved != "" {
		return resolved, nil
	}
	if id != "" {
		return "", notFoundf("no mcp found with id %q", id)
	}
	return "", notFoundf("no mcp found with name %q", name)
}
