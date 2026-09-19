package config

import "encoding/json"

// Clone returns a deep copy of s: no slice, map or pointer in the result
// shares storage with s, so mutating one leaves the other untouched.
//
// This is hand-written rather than a JSON round-trip. A round-trip demands
// that every field marshal successfully, and a field that must refuse to
// marshal at all — because handing back its zero value would be a silent
// leak rather than a copy — would make every single Clone panic. Cloning
// field by field has no such dependency on what the fields are for.
func (s *Settings) Clone() *Settings {
	cp := *s
	cp.ExternalMcps = cloneExternalMcps(s.ExternalMcps)
	cp.Services = cloneServiceConfigs(s.Services)
	cp.Projects = cloneProjects(s.Projects)
	cp.Hosts = cloneHosts(s.Hosts)
	cp.Enrolments = cloneEnrolments(s.Enrolments)
	cp.Audit = cloneAuditConfig(s.Audit)
	cp.Remote = cloneRemoteConfig(s.Remote)
	cp.ModelEndpoint = cloneModelEndpointConfig(s.ModelEndpoint)
	cp.APICredentials = cloneAPICredentials(s.APICredentials)
	cp.LoginBootstrap = cloneLoginBootstrap(s.LoginBootstrap)
	cp.Passkeys = clonePasskeys(s.Passkeys)
	cp.EveEnrolment = cloneEveEnrolmentWindow(s.EveEnrolment)
	cp.EvePasskeys = cloneSlice(s.EvePasskeys)
	cp.EvePasskeyRevocations = cloneSlice(s.EvePasskeyRevocations)
	cp.TerminalTemplates = cloneTerminalTemplates(s.TerminalTemplates)
	return &cp
}

// cloneSlice copies a slice element by element, preserving the nil/non-nil
// distinction: normalize() and a hand-edited settings.json both rely on nil
// meaning something different from empty, and a clone that turned one into
// the other would change what the next save serializes.
func cloneSlice[T any](s []T) []T {
	if s == nil {
		return nil
	}
	out := make([]T, len(s))
	copy(out, s)
	return out
}

func cloneMap[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return nil
	}
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneStringSliceMap(m map[string][]string) map[string][]string {
	if m == nil {
		return nil
	}
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[k] = cloneSlice(v)
	}
	return out
}

func cloneContextMap(m map[string]json.RawMessage) map[string]json.RawMessage {
	if m == nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		out[k] = cloneSlice(v)
	}
	return out
}

func cloneBoolPtr(b *bool) *bool {
	if b == nil {
		return nil
	}
	cp := *b
	return &cp
}

func cloneOAuthState(o *OAuthState) *OAuthState {
	if o == nil {
		return nil
	}
	cp := *o
	return &cp
}

func cloneExternalMcp(m ExternalMcp) ExternalMcp {
	m.Args = cloneSlice(m.Args)
	m.Env = cloneMap(m.Env)
	m.DiscoveredTools = cloneSlice(m.DiscoveredTools)
	m.ContextSchema = cloneSlice(m.ContextSchema)
	m.OAuthState = cloneOAuthState(m.OAuthState)
	m.TccServices = cloneSlice(m.TccServices)
	return m
}

func cloneExternalMcps(s []ExternalMcp) []ExternalMcp {
	if s == nil {
		return nil
	}
	out := make([]ExternalMcp, len(s))
	for i, m := range s {
		out[i] = cloneExternalMcp(m)
	}
	return out
}

func cloneServiceConfig(c ServiceConfig) ServiceConfig {
	c.Args = cloneSlice(c.Args)
	c.Env = cloneMap(c.Env)
	c.Capabilities = cloneSlice(c.Capabilities)
	c.LegacyFrontendConsumer = cloneBoolPtr(c.LegacyFrontendConsumer)
	c.AllowedModels = cloneSlice(c.AllowedModels)
	return c
}

func cloneServiceConfigs(s []ServiceConfig) []ServiceConfig {
	if s == nil {
		return nil
	}
	out := make([]ServiceConfig, len(s))
	for i, c := range s {
		out[i] = cloneServiceConfig(c)
	}
	return out
}

func clonePermissionPolicy(p *PermissionPolicy) *PermissionPolicy {
	if p == nil {
		return nil
	}
	cp := *p
	cp.AllowedTools = cloneSlice(p.AllowedTools)
	cp.DeniedTools = cloneSlice(p.DeniedTools)
	return &cp
}

func cloneShellTemplate(t ShellTemplate) ShellTemplate {
	t.Args = cloneSlice(t.Args)
	t.Env = cloneMap(t.Env)
	return t
}

func cloneTerminalTemplate(t TerminalTemplate) TerminalTemplate {
	t.Args = cloneSlice(t.Args)
	t.Env = cloneMap(t.Env)
	t.EnvPassthrough = cloneSlice(t.EnvPassthrough)
	t.Read = cloneSlice(t.Read)
	t.ReadWrite = cloneSlice(t.ReadWrite)
	t.Deny = cloneSlice(t.Deny)
	return t
}

func cloneTerminalTemplates(s []TerminalTemplate) []TerminalTemplate {
	if s == nil {
		return nil
	}
	out := make([]TerminalTemplate, len(s))
	for i, t := range s {
		out[i] = cloneTerminalTemplate(t)
	}
	return out
}

func cloneShellTemplates(s []ShellTemplate) []ShellTemplate {
	if s == nil {
		return nil
	}
	out := make([]ShellTemplate, len(s))
	for i, t := range s {
		out[i] = cloneShellTemplate(t)
	}
	return out
}

func cloneProject(p Project) Project {
	p.AllowedMcpIDs = cloneSlice(p.AllowedMcpIDs)
	p.AllowedModels = cloneSlice(p.AllowedModels)
	p.ChatTemplates = cloneSlice(p.ChatTemplates)
	p.ShellTemplates = cloneShellTemplates(p.ShellTemplates)
	p.DisabledTools = cloneStringSliceMap(p.DisabledTools)
	p.Context = cloneContextMap(p.Context)
	p.AllowedTools = cloneStringSliceMap(p.AllowedTools)
	p.Access = cloneMap(p.Access)
	p.AllowExternal = cloneMap(p.AllowExternal)
	p.Mounts = cloneSlice(p.Mounts)
	p.PermissionPolicy = clonePermissionPolicy(p.PermissionPolicy)
	p.SessionFolders = cloneSlice(p.SessionFolders)
	return p
}

func cloneProjects(s []Project) []Project {
	if s == nil {
		return nil
	}
	out := make([]Project, len(s))
	for i, p := range s {
		out[i] = cloneProject(p)
	}
	return out
}

func cloneHostProbe(p *HostProbe) *HostProbe {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

func cloneHost(h Host) Host {
	h.Probe = cloneHostProbe(h.Probe)
	return h
}

func cloneHosts(s []Host) []Host {
	if s == nil {
		return nil
	}
	out := make([]Host, len(s))
	for i, h := range s {
		out[i] = cloneHost(h)
	}
	return out
}

func cloneEnrolment(e Enrolment) Enrolment {
	e.ProjectIDs = cloneSlice(e.ProjectIDs)
	return e
}

func cloneEnrolments(s []Enrolment) []Enrolment {
	if s == nil {
		return nil
	}
	out := make([]Enrolment, len(s))
	for i, e := range s {
		out[i] = cloneEnrolment(e)
	}
	return out
}

func cloneAuditConfig(c *AuditConfig) *AuditConfig {
	if c == nil {
		return nil
	}
	cp := *c
	cp.Enabled = cloneBoolPtr(c.Enabled)
	cp.LogArgs = cloneBoolPtr(c.LogArgs)
	cp.LogLists = cloneBoolPtr(c.LogLists)
	cp.RedactKeys = cloneSlice(c.RedactKeys)
	return &cp
}

func cloneRemoteConfig(c *RemoteConfig) *RemoteConfig {
	if c == nil {
		return nil
	}
	cp := *c
	cp.Enabled = cloneBoolPtr(c.Enabled)
	cp.EnrolmentRequests = cloneBoolPtr(c.EnrolmentRequests)
	return &cp
}

func cloneModelEndpointConfig(c *ModelEndpointConfig) *ModelEndpointConfig {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}

func cloneAPICredential(c APICredential) APICredential {
	c.Classes = cloneSlice(c.Classes)
	return c
}

func cloneAPICredentials(s []APICredential) []APICredential {
	if s == nil {
		return nil
	}
	out := make([]APICredential, len(s))
	for i, c := range s {
		out[i] = cloneAPICredential(c)
	}
	return out
}

func cloneLoginBootstrap(b *LoginBootstrap) *LoginBootstrap {
	if b == nil {
		return nil
	}
	cp := *b
	return &cp
}

func cloneEveEnrolmentWindow(w *EveEnrolmentWindow) *EveEnrolmentWindow {
	if w == nil {
		return nil
	}
	cp := *w
	return &cp
}

func clonePasskey(p Passkey) Passkey {
	p.X = cloneSlice(p.X)
	p.Y = cloneSlice(p.Y)
	return p
}

func clonePasskeys(s []Passkey) []Passkey {
	if s == nil {
		return nil
	}
	out := make([]Passkey, len(s))
	for i, p := range s {
		out[i] = clonePasskey(p)
	}
	return out
}
