package main

import "fmt"

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

func revealForUI(s Secret) string {
	if pt, ok := s.Reveal(); ok {
		return pt
	}
	return sealUnavailablePlaceholder
}

func revealEnvForUI(m map[string]Secret) map[string]string {
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
	Project
	Token string `json:"token"`
}

func projectToNativeView(p Project) nativeProject {
	return nativeProject{Project: p, Token: revealForUI(p.Token)}
}

func projectsToNativeView(ps []Project) []nativeProject {
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

func nativeOAuthStateOf(o *OAuthState) *nativeOAuthState {
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
	ExternalMcp
	Env        map[string]string `json:"env"`
	OAuthState *nativeOAuthState `json:"oauth_state,omitempty"`
}

func externalMcpToNativeView(m ExternalMcp) nativeExternalMcp {
	return nativeExternalMcp{
		ExternalMcp: m,
		Env:         revealEnvForUI(m.Env),
		OAuthState:  nativeOAuthStateOf(m.OAuthState),
	}
}

func externalMcpsToNativeView(ms []ExternalMcp) []nativeExternalMcp {
	out := make([]nativeExternalMcp, 0, len(ms))
	for _, m := range ms {
		out = append(out, externalMcpToNativeView(m))
	}
	return out
}

type nativeServiceConfig struct {
	ServiceConfig
	Env map[string]string `json:"env"`
}

func serviceConfigToNativeView(c ServiceConfig) nativeServiceConfig {
	return nativeServiceConfig{ServiceConfig: c, Env: revealEnvForUI(c.Env)}
}

func serviceConfigsToNativeView(cs []ServiceConfig) []nativeServiceConfig {
	out := make([]nativeServiceConfig, 0, len(cs))
	for _, c := range cs {
		out = append(out, serviceConfigToNativeView(c))
	}
	return out
}

// revealEnvOrErr reveals every value of m, refusing if any could not be
// opened. A spawned child must never receive fewer or different variables
// than its record configures, so a value the sealed store cannot currently
// open is refused outright rather than passed through empty or silently
// dropped — the same "everything that needs a sealed value refuses"
// principle §5.6 clause 4 names for ResolvePtyEnv and the admin ops,
// extended to every other consumer of a sealed env value.
func revealEnvOrErr(m map[string]Secret) (map[string]string, error) {
	if m == nil {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		pt, ok := v.Reveal()
		if !ok {
			return nil, fmt.Errorf("env value %q could not be opened: the sealed store is unavailable", k)
		}
		out[k] = pt
	}
	return out, nil
}

// secretMapFromPlain converts an operator-typed (or freshly discovered)
// plaintext env map into the sealed-in-memory representation used on
// ExternalMcp.Env / ServiceConfig.Env. The values are plaintext relay
// itself just received, not something read back off disk, so NewSecret —
// not UnmarshalJSON's legacy path — is the right constructor (§4.3).
func secretMapFromPlain(m map[string]string) map[string]Secret {
	if m == nil {
		return nil
	}
	out := make(map[string]Secret, len(m))
	for k, v := range m {
		out[k] = NewSecret(v)
	}
	return out
}
