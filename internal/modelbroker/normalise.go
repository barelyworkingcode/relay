package modelbroker

import "strings"

// Reasons Allowed returns as its third value. Denied and NotFound produce
// the identical wire 404 (spec §2.4) but must stay distinguishable for
// audit (spec §7) — a project cannot tell them apart, but `relay audit` can.
const (
	ReasonAllowed  = "allowed"
	ReasonDenied   = "denied"
	ReasonNotFound = "not_found"
)

// group classifies a Row for the purpose of which grant-list spellings may
// name it (spec §6.2's table).
type group int

const (
	groupOther group = iota
	groupLlama
	groupMLX
	groupVirtual
	groupModelMap
)

// classify reads a Row's OwnedBy the way relayLLM's own /v1/models writes
// it (router_models.go): "virtual" for a virtual model, "anthropic-map" for
// a modelMap key, and otherwise a manager's config.ServerProfile.Group
// ("llama.cpp", "MLX") or an endpoint's name for everything else. The
// managed-server groups are matched case-insensitively on substring, not
// equality — this is deliberate: the profile Group is an operator-facing
// display label, while the grant-list prefix the spec defines ("llama/",
// "mlx/") mirrors the manager's Kind, not that label, and the label has
// always contained its Kind as a substring in every real profile.
func classify(row Row) group {
	switch row.OwnedBy {
	case "virtual":
		return groupVirtual
	case "anthropic-map":
		return groupModelMap
	}
	lower := strings.ToLower(row.OwnedBy)
	switch {
	case strings.Contains(lower, "llama"):
		return groupLlama
	case strings.Contains(lower, "mlx"):
		return groupMLX
	default:
		return groupOther
	}
}

// findRow looks up a row by exact, case-sensitive id.
func findRow(rows []Row, id string) (Row, bool) {
	for _, row := range rows {
		if row.ID == id {
			return row, true
		}
	}
	return Row{}, false
}

// canonicalize maps a requested model id to the router id relayLLM actually
// dispatches on, accepting the spellings spec §6.2 documents in addition to
// the bare router id itself:
//
//   - a bare id already in the catalog (any row type, including a modelMap
//     key — the router rewrites that itself before dispatch, so forwarding
//     it unchanged is correct);
//   - "llama/<alias>" or "mlx/<alias>", when <alias> is a managed row in the
//     matching group;
//   - "pi/relay-router/<id>", when <id> is a managed, virtual or endpoint
//     row. A modelMap key has no such spelling: the spec's table grants it
//     only through its own bare id or through its target, never through a
//     pi-prefixed name.
//
// This is deliberate, not incidental: relayLLM's router only ever dispatches
// on the bare id (router.go's handleProxy matches managed aliases, virtual
// names and endpoint ids literally). A prefixed spelling that reached the
// router unchanged would 400 as an unknown model, so the broker must resolve
// it before forwarding, not just before checking the grant.
func canonicalize(requested string, rows []Row) (string, Row, bool) {
	if row, ok := findRow(rows, requested); ok {
		return requested, row, true
	}
	if rest, ok := strings.CutPrefix(requested, "llama/"); ok {
		if row, ok := findRow(rows, rest); ok && classify(row) == groupLlama {
			return rest, row, true
		}
	}
	if rest, ok := strings.CutPrefix(requested, "mlx/"); ok {
		if row, ok := findRow(rows, rest); ok && classify(row) == groupMLX {
			return rest, row, true
		}
	}
	if rest, ok := strings.CutPrefix(requested, "pi/relay-router/"); ok {
		if row, ok := findRow(rows, rest); ok && classify(row) != groupModelMap {
			return rest, row, true
		}
	}
	return "", Row{}, false
}

func containsString(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// hasPiGrant reports whether grant holds a pi session spelling of canon:
// "pi/<provider>/<canon>" for any non-empty provider, "relay-router"
// included. A pi session reaches the broker with the bare id whatever
// provider its model id names, so the provider segment does not narrow the
// grant. The provider is the first segment only; everything after it must
// equal canon exactly, so an endpoint id keeps its own slash.
func hasPiGrant(grant []string, canon string) bool {
	for _, e := range grant {
		rest, ok := strings.CutPrefix(e, "pi/")
		if !ok {
			continue
		}
		provider, id, ok := strings.Cut(rest, "/")
		if ok && provider != "" && id == canon {
			return true
		}
	}
	return false
}

// granted decides whether grant covers canon (already resolved to a catalog
// row), applying spec §6.2's per-group spellings. A modelMap row recurses
// onto its target's own row, so allowing a modelMap key is really allowing
// whatever it currently points at — the key itself grants nothing (spec:
// "The key alone grants nothing, since router.go:423-430 rewrites it before
// dispatch").
func granted(canon string, row Row, grant []string, rows []Row) bool {
	if containsString(grant, canon) {
		return true
	}
	switch classify(row) {
	case groupLlama:
		return containsString(grant, "llama/"+canon) || hasPiGrant(grant, canon)
	case groupMLX:
		return containsString(grant, "mlx/"+canon) || hasPiGrant(grant, canon)
	case groupVirtual, groupOther:
		return hasPiGrant(grant, canon)
	case groupModelMap:
		if hasPiGrant(grant, canon) {
			return true
		}
		if row.Target == "" {
			return false
		}
		targetRow, ok := findRow(rows, row.Target)
		if !ok {
			// A modelMap key pointing at a target absent from the current
			// catalog is an operator config error, not a grant the caller
			// can somehow still hold — refuse rather than guess.
			return false
		}
		return granted(row.Target, targetRow, grant, rows)
	default:
		return false
	}
}

// Allowed decides whether a requested model id may be used under grant,
// against the given catalog snapshot. It returns the canonical router id to
// forward upstream, whether the call is allowed, and — when it is not — a
// Reason distinguishing "resolves to a real model, but grant doesn't cover
// it" (ReasonDenied) from "does not resolve to anything in the catalog under
// any accepted spelling" (ReasonNotFound). The wire response for both is the
// identical 404 (spec §2.4); only the audit record tells them apart.
//
// grant's empty-list meaning is the caller's to decide, not this function's:
// a project's empty allowed_models means "any model" and a service's empty
// AllowedModels means "no models" (plan §2 C1). Allowed itself treats an
// empty grant as "nothing granted" and a grant containing the literal entry
// "*" as "every catalog row granted" — a project caller must translate its
// own empty-means-any rule into passing []string{"*"} before calling this
// function; a service caller passes its AllowedModels unchanged, since
// service's empty-means-none already matches Allowed's default.
func Allowed(requested string, grant []string, rows []Row) (canonical string, ok bool, reason string) {
	canon, row, found := canonicalize(requested, rows)
	if !found {
		return "", false, ReasonNotFound
	}
	// This is subtle: "*" short-circuits granted, so it does not recurse
	// into a modelMap row's target the way a specific grant entry would. A
	// wildcard grants every row the catalog lists, full stop — including a
	// modelMap key whose target has gone stale (an operator config error,
	// not a scoping question), since that row is still one relayLLM itself
	// vouches for by listing it.
	if containsString(grant, "*") || granted(canon, row, grant, rows) {
		return canon, true, ReasonAllowed
	}
	return canon, false, ReasonDenied
}
