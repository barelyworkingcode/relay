package main

import "github.com/barelyworkingcode/relay/internal/config"

// The tray's own Settings window is not the eve/HTTP boundary projectView
// exists for (project_dto.go) — it IS relay, the token authority, and
// legitimately shows and rotates a project's plaintext token, and lets an
// operator read and edit an MCP or service's env values. But a raw
// Project, ExternalMcp or ServiceConfig no longer marshals at all once its
// Secret fields hold real values (Secret.MarshalJSON refuses anything
// that is not sealed, by design) — and even where it did, that would hand
// the WebView an AES-GCM envelope instead of the plaintext it needs to
// render. These native*View types are the one place that gap is closed:
// Secret revealed for exactly this one, already-trusted, loopback-only
// surface.

// sealUnavailablePlaceholder is shown instead of a plaintext value the
// sealed store could not open (§5.6 clause 2) — a blank field would read
// as "this project has no token", which is not what a missing key means.
const sealUnavailablePlaceholder = "unavailable — the sealed store cannot be opened"

func revealForUI(s config.Secret) string {
	if pt, ok := s.Reveal(); ok {
		return pt
	}
	return sealUnavailablePlaceholder
}

func revealEnvForUI(m map[string]config.Secret) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = revealForUI(v)
	}
	return out
}

// nativeProject shadows Project's Token field (same field name, so the
// shallower one wins for both directions of encoding/json's promotion
// rule) with its revealed plaintext.
type nativeProject struct {
	config.Project
	Token string `json:"token"`
}

func projectToNativeView(p config.Project) nativeProject {
	return nativeProject{Project: p, Token: revealForUI(p.Token)}
}

func projectsToNativeView(ps []config.Project) []nativeProject {
	out := make([]nativeProject, 0, len(ps))
	for _, p := range ps {
		out = append(out, projectToNativeView(p))
	}
	return out
}

type nativeOAuthState struct {
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenExpiry  string `json:"token_expiry,omitempty"`
}

func nativeOAuthStateOf(o *config.OAuthState) *nativeOAuthState {
	if o == nil {
		return nil
	}
	return &nativeOAuthState{
		ClientID:     o.ClientID,
		ClientSecret: revealForUI(o.ClientSecret),
		AccessToken:  revealForUI(o.AccessToken),
		RefreshToken: revealForUI(o.RefreshToken),
		TokenExpiry:  o.TokenExpiry,
	}
}

type nativeExternalMcp struct {
	config.ExternalMcp
	Env        map[string]string `json:"env"`
	OAuthState *nativeOAuthState `json:"oauth_state,omitempty"`
}

func externalMcpToNativeView(m config.ExternalMcp) nativeExternalMcp {
	return nativeExternalMcp{
		ExternalMcp: m,
		Env:         revealEnvForUI(m.Env),
		OAuthState:  nativeOAuthStateOf(m.OAuthState),
	}
}

func externalMcpsToNativeView(ms []config.ExternalMcp) []nativeExternalMcp {
	out := make([]nativeExternalMcp, 0, len(ms))
	for _, m := range ms {
		out = append(out, externalMcpToNativeView(m))
	}
	return out
}

type nativeServiceConfig struct {
	config.ServiceConfig
	Env map[string]string `json:"env"`
}

func serviceConfigToNativeView(c config.ServiceConfig) nativeServiceConfig {
	return nativeServiceConfig{ServiceConfig: c, Env: revealEnvForUI(c.Env)}
}

func serviceConfigsToNativeView(cs []config.ServiceConfig) []nativeServiceConfig {
	out := make([]nativeServiceConfig, 0, len(cs))
	for _, c := range cs {
		out = append(out, serviceConfigToNativeView(c))
	}
	return out
}
