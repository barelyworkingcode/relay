package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/google/uuid"
)

// CallTool is the one that matters for security review; the list kinds
// record what tool surface a credential was shown.
const (
	AuditEventCallTool   = "call_tool"
	AuditEventListTools  = "list_tools"
	AuditEventListSkills = "list_skills"

	// McpDown and McpUp are not calls. They record that an external MCP's
	// child process died and that it came back (ADR-012) — a deliberate
	// widening of ADR-008's remit from "what a credential attempted" to "what
	// relay could serve at all". They belong in THIS log, not the app log,
	// because a dead MCP is the state in which every gated call fails for a
	// reason that has nothing to do with the grant, and without these rows the
	// log shows a run of `error` outcomes with no cause.
	AuditEventMcpDown = "mcp_down"
	AuditEventMcpUp   = "mcp_up"

	// control.ControlDecision is a control-plane authorization outcome, ALLOWED and
	// REFUSED alike, given the same standing as a tool-call denial (ADR-015).
	// Built by audit_control.go, never by router instrumentation.
	AuditEventControlDecision = "control_decision"

	// CredentialIssued and CredentialRevoked record that a credential came
	// into existence or stopped existing. They are a different fact from a
	// control_decision, which says a caller was allowed to reach a route:
	// most issuance is initiated from a CLI process that reaches no route at
	// all, and the routes that do issue would otherwise record the
	// authorization and not the act. Built by audit_issuance.go.
	AuditEventCredentialIssued  = "credential_issued"
	AuditEventCredentialRevoked = "credential_revoked"

	// ConfigChange records a gated act that mutates settings but issues
	// nothing: registering or unregistering an MCP or service, starting an
	// MCP's OAuth flow, or widening a project's grant shape. Calling one of
	// these credential_issued would be a lie, and leaving it unrecorded
	// would break the ADR's detection argument, which depends on every
	// gated act leaving a record (ADR-017 implementation spec §7.5).
	AuditEventConfigChange = "config_change"

	// MountAttach, MountOp and MountDetach are the 9P mount plane's three
	// event kinds: the one attach record written durably before any 9P byte
	// is served, the mutation and refusal rows a live session writes, and
	// the one close record with the session's running totals.
	AuditEventMountAttach = "mount_attach"
	AuditEventMountOp     = "mount_op"
	AuditEventMountDetach = "mount_detach"

	// HostProbe records one ssh probe of a Host (docs/ssh-hosts.md): outcome
	// ok/error, the target reached, and (on success) the paths and versions
	// discovered — Args carries that detail rather than a new field per
	// datum, the same choice ADR-011's per-call Scope makes for structured
	// per-event detail relay does not otherwise need to query on.
	AuditEventHostProbe = "host.probe"

	// ModelCall and ModelList are the model endpoint's two event kinds
	// (docs/model-endpoint.md, spec-model-broker.md §7): one per finished
	// call to a model route, and one per GET /v1/models listing (recorded
	// only when LogLists is on, the same gate list_tools/list_skills use).
	// R-M1c (plan-broker-and-sessions.md §2 C4) is the unit that wires these
	// into cmd/relay/model_endpoint.go's AuditHook; both are emitted by it.
	AuditEventModelCall = "model_call"
	AuditEventModelList = "model_list"

	// SessionLaunch, SessionBound, SessionEnd and SessionResume are
	// plan-broker-and-sessions.md §2 C4's session-host event kinds. Added
	// here, up front, so the later unit that emits them (a session-host root
	// process, `relay-sessions`) never has to edit this file again — nothing
	// in this repo constructs one of these yet.
	AuditEventSessionLaunch = "session_launch"
	AuditEventSessionBound  = "session_bound"
	AuditEventSessionEnd    = "session_end"
	AuditEventSessionResume = "session_resume"
)

// Denied means a known credential was refused a tool it may not use;
// Unauthorized means the credential itself did not resolve — they call for
// different responses.
//
// Error means the call did not complete (transport failure, MCP unreachable);
// ToolError means the call completed and the MCP answered "no" via a normal
// result carrying isError, e.g. a path outside allowed_dirs. Only the second
// tells you a boundary was probed and held.
//
// Throttled means the grant was legitimate and the tool was allowed, but the
// *pattern of use* was refused — a rate or volume budget on the enrolment was
// exceeded (ADR-010 decision 7). That is what exfiltration looks like from the
// host's side, so it must not be flattened into denied.
//
// Pending is a real value rather than an empty string because "outcome" is a
// non-omitempty on-disk field every consumer already reads (CLI table, UI
// pill, --outcome filter); "" would render as nothing at all.
const (
	AuditOutcomeOK           = "ok"
	AuditOutcomeError        = "error"
	AuditOutcomeToolError    = "tool_error"
	AuditOutcomeDenied       = "denied"
	AuditOutcomeUnauthorized = "unauthorized"
	AuditOutcomeThrottled    = "throttled"
	AuditOutcomePending      = "pending"

	// NotFound and ClientAbort are the model endpoint's additions to this
	// vocabulary (plan-broker-and-sessions.md §2 C4). NotFound is the
	// audit-only half of the identical-404 pair the model endpoint returns
	// for a model outside the caller's grant (Denied) versus one absent from
	// the catalog entirely (NotFound) — the wire response never lets a
	// caller tell the two apart, this field does. ClientAbort mirrors
	// Error/ToolError's split for tool calls: the caller went away rather
	// than the call failing.
	AuditOutcomeNotFound    = "not_found"
	AuditOutcomeClientAbort = "client_abort"
)

// A local call is one record and carries no phase at all, keeping every line
// written for a local caller the shape ADR-008 specified. A remote call is
// two records sharing one event id — an intent written and flushed before the
// MCP is invoked, and a completion written when the call returns.
const (
	AuditPhaseIntent     = "intent"
	AuditPhaseCompletion = "completion"
)

// Remote is a distinct value rather than a reuse of Project so that "show me
// everything any VM did" is a first-class filter — a remote caller is remote
// *and* acting as a project grant, and both facts are recorded (ADR-010
// decision 6).
const (
	AuditActorProject = "project"
	AuditActorService = "service"
	AuditActorRemote  = "remote"
	AuditActorUnknown = "unknown"

	// Relay is the actor on a record relay wrote about itself rather than
	// about a caller (today only the mcp_down / mcp_up rows), so `--kind
	// relay` selects them as a set.
	AuditActorRelay = "relay"

	// Control is the credential that reached the frontend control plane
	// (ADR-015) — a distinct actor kind from Project/Service/Remote because
	// it names a capability-classed credential, not a tool caller.
	AuditActorControl = "control"

	// Operator is whoever owns the config dir: the `relay` CLI and the tray's
	// own windows. It is distinct from Control because there is no credential
	// to name — the authorization is ownership of the config dir — and
	// distinct from Relay because relay is not acting on its own behalf.
	AuditActorOperator = "operator"

	// ProjectSession is plan-broker-and-sessions.md §2 C1/C2's session-host
	// identity kind — a session-host root process or one of its descendants,
	// acting for its own project. Reserved here so R-M1c's constant addition
	// covers it up front; nothing in this repo resolves this identity kind
	// yet (docs/model-endpoint.md's "What is not brokered" names the same
	// gap for the model endpoint specifically).
	AuditActorProjectSession = "project_session"
)

const (
	AuditAuthToken = "token"
	// Cwd named directory auth, which is retired (plan-broker-and-sessions.md
	// §2 C3): relay no longer writes it. This is deliberate — it stays as the
	// name for a value that exists in audit files already on disk, which
	// `relay audit` still reads. Nothing may write it again; session
	// membership is what replaced it.
	AuditAuthCwd     = "cwd"
	AuditAuthService = "service"
	AuditAuthMTLS    = "mtls"
	AuditAuthNone    = "none"

	// ModelKey is a caller authenticated by an rmk_-prefixed model key
	// (docs/model-endpoint.md's Model keys section) rather than a project's
	// own token — a real distinction because a key is revocable independently
	// of the project token it was minted under.
	AuditAuthModelKey = "model_key"

	// Session is plan-broker-and-sessions.md §2 C1/C2's session-host
	// authentication kind, recorded alongside AuditActorProjectSession for a
	// caller admitted as a session's root process or a kernel-verified
	// descendant of one (C3).
	AuditAuthSession = "session"
)

// Every field is derived from relay's own resolution or the kernel, never
// from a value the caller supplied. Cwd is the one exception and is no
// longer written at all: it carried the caller-asserted working directory
// of the retired directory auth, and remains only to read records that
// already hold one (see AuditAuthCwd).
type AuditActor struct {
	Kind        string `json:"kind"`
	ProjectID   string `json:"project_id,omitempty"`
	ProjectName string `json:"project_name,omitempty"`
	Auth        string `json:"auth"`
	Cwd         string `json:"cwd,omitempty"`

	// Omitempty so an absent field reads as "not applicable" instead of
	// "unknown" for a remote caller.
	PID    int    `json:"pid,omitempty"`
	Proc   string `json:"proc,omitempty"`
	Parent string `json:"parent,omitempty"`

	// Fingerprint is recorded alongside the resolved client id rather than
	// instead of it: that keeps a revoked device's history legible, answering
	// which *key* made a call after the enrolment naming it is deleted
	// (ADR-010 decision 6).
	ClientID    string `json:"client_id,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	RemoteAddr  string `json:"remote_addr,omitempty"`

	// CredID identifies the control-plane credential (ADR-015). Deliberately
	// never the token or its hash — control.ControlDecision has no such field to
	// leak, and this must stay that way.
	CredID string `json:"cred_id,omitempty"`

	// SessionID names the project_session that vouched for a caller
	// (plan-broker-and-sessions.md §2 C3/C4): the session whose root process
	// made the call, or whose descendant the kernel placed the caller in.
	// Never a secret — a session id is relay-minted, held by the session's
	// own processes, and load-bearing for reading an audit trail.
	SessionID string `json:"session_id,omitempty"`

	// ServiceID names a service actor's launch identity id on a model
	// endpoint call (docs/model-endpoint.md). A tool-call service actor is
	// identified by PID/Proc instead; the model endpoint's own audit hook
	// (cmd/relay/model_endpoint.go's ModelCallAudit) does not thread the
	// socket peer's pid through, so the launch identity's own id is what
	// names the caller there.
	ServiceID string `json:"service_id,omitempty"`
}

// Field names are the on-disk contract: external tooling greps this file.
type AuditEvent struct {
	ID    string     `json:"id"`
	TS    time.Time  `json:"ts"`
	DurMs int64      `json:"dur_ms"`
	Event string     `json:"event"`
	Actor AuditActor `json:"actor"`

	// Empty for the single record a local call produces; "intent" or
	// "completion" for the two records (sharing this ID) a remote call
	// produces.
	Phase string `json:"phase,omitempty"`

	McpID string `json:"mcp_id,omitempty"`
	Tool  string `json:"tool,omitempty"`

	// When ArgsTruncated is set, Args holds a JSON *string* containing the
	// truncated prefix rather than the original object, so the line stays
	// valid JSON either way.
	Args          json.RawMessage `json:"args,omitempty"`
	ArgsBytes     int             `json:"args_bytes,omitempty"`
	ArgsTruncated bool            `json:"args_truncated,omitempty"`

	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`

	ResultBytes   int    `json:"result_bytes,omitempty"`
	ResultIsError bool   `json:"result_is_error,omitempty"`
	ResultPreview string `json:"result_preview,omitempty"`

	// Access and Scope record the AUTHORITY the call ran with, not just the
	// grant it ran under (ADR-011 decision 7): the grant PLUS the mode PLUS
	// the injected scope. Re-reading settings.json at query time would answer
	// a different question once an operator has since edited the profile.
	//
	// Scope carries ONLY the fields the MCP declared as scope: "restrict",
	// never the whole per-MCP context map — _meta is a general channel and a
	// future MCP may pass an API key through it, so logging it wholesale
	// would make this file the place credentials go to be archived.
	Access string `json:"access,omitempty"`

	// Deliberately NOT omitempty. A nil map and an empty, non-nil map are
	// different facts on the wire — nil means this MCP declares no
	// scope: "restrict" field at all, an empty map means it does and this
	// call's grant supplied no value for it (itself the finding on a `denied`
	// record; ADR-011 decision 4) — and omitempty treats both as "empty" for
	// a map, erasing the distinction. encoding/json renders a nil map as
	// `null` and a non-nil empty one as `{}`, giving the three-way split
	// (absent / declared-empty / populated) this field needs for free.
	Scope map[string]json.RawMessage `json:"scope"`

	// Names every field the GRANT set a value for that the MCP's live schema
	// does not declare, so relay could not place it and refused the call.
	//
	// A field of its own rather than a fourth reading of Scope: Scope's
	// readings are about what the MCP declares, this is about what the
	// OPERATOR declared. Folding it into `scope: null` would conflate "this
	// MCP declares no scope field" with "this MCP declares none of the
	// fields your profile set" — the conflation that let an unconfined
	// dispatch read, in the log, as an MCP that was never scoped at all.
	ScopeUnplaced []string `json:"scope_unplaced,omitempty"`

	// The resolved --root directory relay spawned this MCP with, when it did
	// (fsMCP v3 integration, R2). A DIFFERENT fact from Scope and must not be
	// read as filling in for it: fsMCP v3 publishes no contextSchema at all,
	// so Scope stays nil — "(none declared)" — on every one of its calls, and
	// that stays true. Empty means relay did not spawn this MCP with --root.
	McpRoot string `json:"mcp_root,omitempty"`

	// The other half of the authority relay decided by itself (ADR-011
	// decision 2c): whether this grant could call a tool reaching outside the
	// host.
	//
	// A POINTER, unlike Access, because the value that matters most is the
	// FALSE one: the resting state, the one a read-only profile has. With a
	// plain bool and omitempty, "the grant was not given" and "nobody
	// recorded the grant" would be the same absent key — and the second is a
	// real state (a service's launch identity bypasses every check in checkToolAccess, and
	// list events carry no MCP at all). Nil means not recorded; false means
	// refused by default.
	AllowExternal *bool `json:"allow_external,omitempty"`

	// Marks a tool_error the MCP labelled as a scope refusal (see
	// scopeViolationMarker). A FIELD and not an outcome on purpose: ADR-008
	// already places this case (tool_error = boundary probed and held), and a
	// scope violation is decided inside the MCP, not by relay, unlike
	// `throttled` (ADR-010), which relay decides with relay's own numbers.
	ScopeViolation bool `json:"scope_violation,omitempty"`

	ToolCount int `json:"tool_count,omitempty"`

	// Set ONLY on mcp_down / mcp_up events, naming the transition: down,
	// restarted, or abandoned (the McpHealth* constants). A field of its own
	// rather than a reuse of Error because two of the three are not errors —
	// `restarted` is the good news. Error is still set beside it when there
	// is a cause to name, making `down: read response: EOF` one sentence.
	Supervision string `json:"supervision,omitempty"`

	// Set only on control_decision events (ADR-015): the route an
	// authorization decision was about. Reason for a refusal rides in Error,
	// same as every other outcome this log records.
	//
	// Class and Transport are plain strings rather than control's
	// CapabilityClass/Transport types — this file's on-disk shape does not
	// depend on the authorization package's types.
	//
	// Method and Path are read off the request line before any class check
	// runs, so a credential with no class at all — "inert rather than
	// omnipotent" per ADR-015 decision 3 — can still shape them, including on
	// its own refusals. audit_control.go caps both at the point this record
	// is built; MethodTruncated/PathTruncated is how that cut stays visible
	// to an operator rather than reading as a short, genuine value.
	Method          string `json:"method,omitempty"`
	MethodTruncated bool   `json:"method_truncated,omitempty"`
	Path            string `json:"path,omitempty"`
	PathTruncated   bool   `json:"path_truncated,omitempty"`
	Class           string `json:"class,omitempty"`
	Transport       string `json:"transport,omitempty"`

	// Set only on credential_issued / credential_revoked events. Credential
	// names WHAT (the auditCredential* vocabulary), Subject its identifier,
	// SubjectName the human-readable label where the kind has one distinct
	// from its identifier, Grants the class set or the granted
	// access-profile ids where the kind has either, and Via how the act was
	// initiated.
	//
	// None of these is ever a plaintext, a hash, or key material:
	// CredentialIssuance has no field that could carry one, which is what
	// makes that true by construction rather than by discipline at each of
	// the doors that build one.
	Credential  string   `json:"credential,omitempty"`
	Subject     string   `json:"subject,omitempty"`
	SubjectName string   `json:"subject_name,omitempty"`
	Grants      []string `json:"grants,omitempty"`
	Via         string   `json:"via,omitempty"`

	// One marker for the four fields above rather than one each, unlike
	// MethodTruncated/PathTruncated: they describe a single act, and the
	// question an operator has of a cut record is whether it was cut, not
	// which part of one sentence lost bytes.
	IssuanceTruncated bool `json:"issuance_truncated,omitempty"`

	// BytesRead, BytesWritten and Ops appear only on a mount_detach row — the
	// session's running totals at close. omitempty so every event kind that
	// isn't a mount_detach round-trips exactly as it does today.
	BytesRead    int64 `json:"bytes_read,omitempty"`
	BytesWritten int64 `json:"bytes_written,omitempty"`
	Ops          int64 `json:"ops,omitempty"`

	// PresenceID is the nonce id (presence.Grant.ID()) that authorised a
	// gated act, on credential_issued, credential_revoked and config_change
	// alike. It is a nonce id, not a secret: it carries no plaintext and no
	// hash, and it lets an operator confirm that a credential issuance has
	// a matching presence event — the absence of one is the signal ADR-017
	// exists to make detectable. Empty for an ungated record.
	PresenceID string `json:"presence_id,omitempty"`

	// Set only on model_call / model_list events (spec-model-broker.md §7,
	// docs/model-endpoint.md's Audit section). ModelKeyLabel names the rmk_
	// key's label the caller authenticated with, never the key itself —
	// ModelCallAudit's own doc comment makes the same promise, and there is
	// no field here that could carry the key, a project token, or either
	// one's hash.
	//
	// Model, ModelCanonical and ModelTarget are three distinct facts, not
	// one field read three ways: Model is what the caller asked for, before
	// any grant check; ModelCanonical is what relay resolved it to against
	// its own catalog (spec-model-broker.md's normalisation); ModelTarget is
	// relayLLM's own account of which managed alias, endpoint or resolved
	// virtual candidate actually served the call, read from its
	// X-Relay-Model-Target response header. A call relay refused before
	// reaching relayLLM carries the first two and never the third.
	ModelKeyLabel  string `json:"model_key_label,omitempty"`
	Model          string `json:"model,omitempty"`
	ModelCanonical string `json:"model_canonical,omitempty"`
	ModelTarget    string `json:"model_target,omitempty"`
	Stream         bool   `json:"stream,omitempty"`

	// RequestBytes and ResponseBytes are counted in relay, not asserted by
	// either endpoint of the proxied call. Separate names from ArgsBytes/
	// ResultBytes rather than reusing them: those two are tool-call-shaped
	// (redacted argument bytes, a result's size) and a model call has
	// neither redaction nor a result value to size.
	RequestBytes  int64 `json:"request_bytes,omitempty"`
	ResponseBytes int64 `json:"response_bytes,omitempty"`

	// PromptTokens and CompletionTokens are parsed from the upstream
	// response by internal/modelbroker/usage.go, when present — never the
	// response content itself, which this file must never carry
	// (spec-model-broker.md §7's "never recorded" list).
	PromptTokens     int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens int64 `json:"completion_tokens,omitempty"`

	// Status is the HTTP status relay answered the caller with. Set only on
	// model_call / model_list; every other event kind's status is implied by
	// Outcome instead.
	Status int `json:"status,omitempty"`
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// Booleans are pointers so an absent block, an absent field, and an explicit
// false are all distinguishable — absent means "use the default", which for
// Enabled is true. Zero-valued ints likewise mean "default"; call resolve()
// before use.
type AuditConfig struct {
	Enabled               *bool    `json:"enabled,omitempty"`
	LogArgs               *bool    `json:"log_args,omitempty"`
	LogLists              *bool    `json:"log_lists,omitempty"`
	MaxArgBytes           int      `json:"max_arg_bytes,omitempty"`
	MaxResultPreviewBytes int      `json:"max_result_preview_bytes,omitempty"`
	RingSize              int      `json:"ring_size,omitempty"`
	MaxFileBytes          int64    `json:"max_file_bytes,omitempty"`
	Generations           int      `json:"generations,omitempty"`
	RedactKeys            []string `json:"redact_keys,omitempty"`
}

// Results are metadata-only by default (preview 0): a tool result carries
// file contents, mail bodies, and calendar entries, and the audit log should
// not quietly become the most sensitive file on the machine. List events
// default off: skill regeneration lists the tool surface for every project on
// every MCP reconcile, which would bury the calls that matter.
const (
	auditDefaultMaxArgBytes  = 4096
	auditDefaultRingSize     = 1000
	auditDefaultMaxFileBytes = 32 << 20
	auditDefaultGenerations  = 5

	// Past this, events are dropped and counted rather than made to wait: the
	// audit sink must never be able to stall a tool call.
	AuditQueueSize = 512

	// Bounds work per query regardless of how large the log has grown.
	AuditTailBudget = 8 << 20
)

type resolvedAuditConfig struct {
	Enabled               bool
	LogArgs               bool
	LogLists              bool
	MaxArgBytes           int
	MaxResultPreviewBytes int
	RingSize              int
	MaxFileBytes          int64
	Generations           int
	RedactKeys            []string
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func intOr(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// A nil receiver resolves to the full default set, so settings.json written
// before this feature existed behaves as if auditing was always on.
func ResolveAuditConfig(c *config.AuditConfig) resolvedAuditConfig {
	if c == nil {
		c = &config.AuditConfig{}
	}
	maxFile := c.MaxFileBytes
	if maxFile <= 0 {
		maxFile = auditDefaultMaxFileBytes
	}
	return resolvedAuditConfig{
		Enabled:               boolOr(c.Enabled, true),
		LogArgs:               boolOr(c.LogArgs, true),
		LogLists:              boolOr(c.LogLists, false),
		MaxArgBytes:           intOr(c.MaxArgBytes, auditDefaultMaxArgBytes),
		MaxResultPreviewBytes: c.MaxResultPreviewBytes, // 0 is meaningful: metadata only
		RingSize:              intOr(c.RingSize, auditDefaultRingSize),
		MaxFileBytes:          maxFile,
		Generations:           intOr(c.Generations, auditDefaultGenerations),
		RedactKeys:            c.RedactKeys,
	}
}

const AuditRedactedValue = "[redacted]"

// Matched as case-insensitive substrings of an argument key, so "mcp_token",
// "X-Api-Key", and "userPassword" are all caught without enumerating every
// spelling. Bare "session" is deliberately absent: session ids are routinely
// load-bearing for debugging and are not secrets, while "session_token" is
// already covered by "token".
var auditSensitiveKeys = []string{
	"token", "secret", "password", "passwd", "apikey", "api_key", "api-key",
	"authorization", "credential", "privatekey", "private_key", "cookie",
	"bearer", "passphrase",
}

var auditRedactedJSON = json.RawMessage(`"` + AuditRedactedValue + `"`)

// Walks JSON *as bytes* and never decodes a value into a Go interface{}
// (ADR-012). The predecessor decoded into interface{}, redacted, and
// re-encoded — which meant the record was Go's paraphrase of what the caller
// sent rather than the caller's own bytes: an unpaired UTF-16 surrogate came
// back as U+FFFD, object keys came back sorted, duplicate keys came back as
// one, numbers came back in Go's float formatting.
//
// It is deliberately total rather than fallible: anything it cannot walk
// (after the json.Compact in RedactArgs, nothing that is valid JSON) is
// returned unchanged rather than dropped. An object is the only shape that
// can carry a key to redact by, so the object walk is the only branch that
// can fail closed: a walk that errors mid-way falls back to the un-redacted
// bytes only when no key was sensitive, and otherwise to the whole value
// replaced.
func redactRaw(raw json.RawMessage, extra []string) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	switch raw[0] {
	case '{':
		return redactRawObject(raw, extra)
	case '[':
		var elems []json.RawMessage
		if err := json.Unmarshal(raw, &elems); err != nil {
			return raw
		}
		out := make([]byte, 0, len(raw))
		out = append(out, '[')
		for i, e := range elems {
			if i > 0 {
				out = append(out, ',')
			}
			out = append(out, redactRaw(e, extra)...)
		}
		return append(out, ']')
	default:
		// A string, number, boolean or null: these bytes are exactly what the
		// caller sent.
		return raw
	}
}

// Rebuilds a JSON object from its own bytes: each key is copied from the
// source rather than re-encoded from the decoded Go string, so key order,
// duplicate keys and any escape a key contains all survive. The decoded key
// is used only to ASK whether the key looks like a credential.
func redactRawObject(raw json.RawMessage, extra []string) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil { // the opening brace
		return raw
	}
	out := make([]byte, 0, len(raw))
	out = append(out, '{')
	first := true
	sawSensitive := false
	for dec.More() {
		keyFrom := dec.InputOffset()
		tok, err := dec.Token()
		if err != nil {
			return redactRawFallback(raw, sawSensitive)
		}
		key, ok := tok.(string)
		if !ok {
			return redactRawFallback(raw, sawSensitive)
		}
		keyTo := dec.InputOffset()
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return redactRawFallback(raw, sawSensitive)
		}
		rawKey := rawKeyBytes(raw, keyFrom, keyTo)
		if len(rawKey) == 0 {
			// Unreachable on compacted input — only a brace or a comma
			// separates a value from the next key — but an empty key would
			// emit `{:v}`, and every line in this log must stay parseable.
			return redactRawFallback(raw, sawSensitive)
		}
		if !first {
			out = append(out, ',')
		}
		first = false
		out = append(out, rawKey...)
		out = append(out, ':')
		if isSensitiveKey(key, extra) {
			sawSensitive = true
			out = append(out, auditRedactedJSON...)
			continue
		}
		out = append(out, redactRaw(val, extra)...)
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return redactRawFallback(raw, sawSensitive)
	}
	return append(out, '}')
}

// Answers the only question a half-walked object leaves: has a credential
// already been seen inside it? If so the partial output cannot be trusted to
// hold the rest, and the whole value is replaced.
func redactRawFallback(raw json.RawMessage, sawSensitive bool) json.RawMessage {
	if sawSensitive {
		return auditRedactedJSON
	}
	return raw
}

// from is the decoder's offset before the key token (a brace or a comma) and
// to is just past the key's closing quote.
func rawKeyBytes(raw json.RawMessage, from, to int64) json.RawMessage {
	i := int(from)
	for i < int(to) && raw[i] != '"' {
		i++
	}
	return raw[i:int(to)]
}

func isSensitiveKey(key string, extra []string) bool {
	lower := strings.ToLower(key)
	for _, s := range auditSensitiveKeys {
		if strings.Contains(lower, s) {
			return true
		}
	}
	for _, s := range extra {
		if s != "" && strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// Over the cap, the result is a JSON string holding the truncated prefix
// (truncated=true) rather than a malformed object, so every line parses.
// Arguments that aren't valid JSON are stored as a capped string too, for a
// faithful record of what was attempted, including malformed attempts.
//
// Under the cap, what is stored is the caller's own bytes with credential
// values replaced — not a re-encoding of them (ADR-012). json.Compact is the
// only rewrite: it strips insignificant whitespace and leaves every string,
// escape and number spelling exactly as the caller wrote it, so the recorded
// arguments and the arguments the MCP received are the same bytes.
func RedactArgs(raw json.RawMessage, maxBytes int, extra []string) (out json.RawMessage, size int, truncated bool) {
	if len(raw) == 0 {
		return nil, 0, false
	}
	size = len(raw)

	// Compact both validates and normalises whitespace; it does not decode, so
	// a lone surrogate escape is still six bytes of ASCII on the other side.
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return capAsString(string(raw), maxBytes)
	}
	encoded := redactRaw(json.RawMessage(buf.Bytes()), extra)
	if len(encoded) <= maxBytes {
		return encoded, size, false
	}
	return capAsString(string(encoded), maxBytes)
}

// Truncates on a rune boundary.
func capAsString(s string, maxBytes int) (json.RawMessage, int, bool) {
	size := len(s)
	if len(s) > maxBytes {
		s = TruncateRunes(s, maxBytes)
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return nil, size, true
	}
	return encoded, size, size > maxBytes
}

func TruncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8StartByte(s[n]) {
		n--
	}
	return s[:n]
}

func utf8StartByte(b byte) bool { return b&0xC0 != 0x80 }

// A fixed-size circular buffer of the most recent events. Backs the settings
// UI's first paint and live tail without re-reading the log file.
type auditRing struct {
	mu   sync.RWMutex
	buf  []AuditEvent
	next int
	full bool
}

// newAuditRing clamps size to at least 1: add's modulo-by-len(r.buf) would
// divide by zero for a zero size, and make would panic outright for a
// negative one, either of which a hand-edited settings.json's ring_size can
// otherwise reach.
func newAuditRing(size int) *auditRing {
	if size < 1 {
		size = 1
	}
	return &auditRing{buf: make([]AuditEvent, size)}
}

func (r *auditRing) add(ev AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = ev
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

// Returns the buffered events newest-first.
func (r *auditRing) snapshot() []AuditEvent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := r.next
	if r.full {
		n = len(r.buf)
	}
	out := make([]AuditEvent, 0, n)
	for i := 0; i < n; i++ {
		// Walk backwards from the most recently written slot.
		idx := (r.next - 1 - i + len(r.buf)*2) % len(r.buf)
		out = append(out, r.buf[idx])
	}
	return out
}

// Writes are handed to a single goroutine over a bounded channel so a slow or
// full disk can never delay a tool call; on overflow the event is dropped and
// counted.
//
// A nil *AuditRecorder is a valid no-op recorder: every method tolerates it,
// so callers never have to branch on whether auditing is configured.
type AuditRecorder struct {
	cfg  resolvedAuditConfig
	path string

	ch      chan AuditEvent
	flushCh chan chan struct{}
	syncCh  chan auditDurableWrite
	ring    *auditRing
	w       io.WriteCloser

	dropped atomic.Uint64
	wrote   atomic.Uint64

	// Guarded by sinkMu because the settings window can attach and detach at
	// any time.
	sinkMu sync.RWMutex
	sink   func(AuditEvent)

	closeOnce sync.Once
	done      chan struct{}
}

// OpenWriter opens the rotating log destination a recorder writes to. Log
// rotation (cmd/relay/log_rotate.go) is shared by relay's own log, the audit
// log and every managed service's log, so this package does not own it —
// callers supply it, the same way service.Registry takes OpenLog.
type OpenWriter func(path string, maxBytes int64, generations int) (io.WriteCloser, error)

// A disabled config returns nil, which every call site treats as
// "auditing off".
func NewAuditRecorder(cfg *config.AuditConfig, path string, openWriter OpenWriter) (*AuditRecorder, error) {
	resolved := ResolveAuditConfig(cfg)
	if !resolved.Enabled {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("audit log dir: %w", err)
	}
	w, err := openWriter(path, resolved.MaxFileBytes, resolved.Generations)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	return NewAuditRecorderWith(resolved, path, w), nil
}

// NewAuditRecorderWith is the constructor a caller that owns the sink uses:
// the tray hands it a rotatingWriter, a CLI process hands it a plain
// append-only file (issuance.go), and a test hands it whatever it needs
// to fail.
func NewAuditRecorderWith(resolved resolvedAuditConfig, path string, w io.WriteCloser) *AuditRecorder {
	// Lowercase once rather than per call.
	extra := make([]string, 0, len(resolved.RedactKeys))
	for _, k := range resolved.RedactKeys {
		extra = append(extra, strings.ToLower(strings.TrimSpace(k)))
	}
	resolved.RedactKeys = extra

	r := &AuditRecorder{
		cfg:     resolved,
		path:    path,
		ch:      make(chan AuditEvent, AuditQueueSize),
		flushCh: make(chan chan struct{}),
		syncCh:  make(chan auditDurableWrite),
		ring:    newAuditRing(resolved.RingSize),
		w:       w,
		done:    make(chan struct{}),
	}
	go r.run()
	return r
}

func (r *AuditRecorder) Enabled() bool { return r != nil && r.cfg.Enabled }

// hasSink reports whether there is a writer goroutine and a file behind this
// recorder.
//
// This is subtle: a zero-valued AuditRecorder answers Enabled() from its
// config alone, and every channel on it is nil. Handing an event to one over
// syncCh would block forever, since a nil channel is never ready and there is
// no run() to close done — so the durable path asks this first and reports the
// recorder unavailable instead of deadlocking its caller.
func (r *AuditRecorder) hasSink() bool { return r != nil && r.syncCh != nil }

// Ready reports whether this recorder actually has somewhere to write --
// enabled AND holding a live sink. cmd/relay's requireIssuanceAuditor uses
// this rather than Enabled alone: an operator can flip
// "enabled": true in settings.json while the recorder actually constructed
// at startup failed to open its file, and the two states must not be
// conflated into "auditing is on".
func (r *AuditRecorder) Ready() bool { return r.Enabled() && r.hasSink() }

func (r *AuditRecorder) LogArgs() bool { return r != nil && r.cfg.LogArgs }

func (r *AuditRecorder) LogLists() bool { return r != nil && r.cfg.LogLists }

// RedactCallArgs prepares one tool call's arguments for the audit record, per
// this recorder's own config: nil when logging arguments is off, otherwise
// redacted and capped. cmd/relay's router instrumentation (audit_call.go)
// calls this rather than reaching cfg's fields directly — cfg stays
// unexported, and this is the operation on it that a caller outside the
// package needs, not a getter for its fields.
func (r *AuditRecorder) RedactCallArgs(args json.RawMessage) (out json.RawMessage, size int, truncated bool) {
	if r == nil || !r.cfg.LogArgs {
		return nil, 0, false
	}
	return RedactArgs(args, r.cfg.MaxArgBytes, r.cfg.RedactKeys)
}

// PreviewResult returns a capped preview of a tool result, or "" when preview
// is off (the default) or there is nothing to preview.
func (r *AuditRecorder) PreviewResult(result json.RawMessage) string {
	if r == nil || r.cfg.MaxResultPreviewBytes <= 0 || len(result) == 0 {
		return ""
	}
	return TruncateRunes(string(result), r.cfg.MaxResultPreviewBytes)
}

func (r *AuditRecorder) Path() string {
	if r == nil {
		return ""
	}
	return r.path
}

// Surfaced in the UI: a nonzero count means the log is incomplete, and an
// incomplete audit log that looks complete is worse than no audit log.
func (r *AuditRecorder) Dropped() uint64 {
	if r == nil {
		return 0
	}
	return r.dropped.Load()
}

// Pass nil to detach.
func (r *AuditRecorder) SetSink(fn func(AuditEvent)) {
	if r == nil {
		return
	}
	r.sinkMu.Lock()
	r.sink = fn
	r.sinkMu.Unlock()
}

// The reply channel is buffered so the writer never blocks on a caller that
// has given up waiting.
type auditDurableWrite struct {
	ev    AuditEvent
	reply chan error
}

// An error rather than a silent success because the only caller is the
// fail-closed path: "I could not record this" and "I recorded this" must
// never be indistinguishable there.
var errAuditUnavailable = errors.New("audit recorder unavailable")

// Blocks until the event is on disk, returning any error rather than
// swallowing it. This is the fail-closed half of the sink, used for a remote
// caller's intent record (ADR-010 decision 5): the call is refused when this
// fails.
//
// Does not use the bounded queue: that exists so a slow sink can never delay
// a *local* tool call, and delaying the call is exactly the point here — but
// the file still belongs to the writer goroutine, so the event is handed over
// rather than written from the caller's goroutine.
func (r *AuditRecorder) RecordDurable(ev AuditEvent) error {
	if !r.hasSink() || !r.cfg.Enabled {
		return errAuditUnavailable
	}
	req := auditDurableWrite{ev: ev, reply: make(chan error, 1)}
	select {
	case r.syncCh <- req:
	case <-r.done:
		return errAuditUnavailable
	}
	select {
	case err := <-req.reply:
		return err
	case <-r.done:
		return errAuditUnavailable
	}
}

// Never blocks: a full queue drops the event and increments the drop counter.
func (r *AuditRecorder) Record(ev AuditEvent) {
	if r == nil || !r.cfg.Enabled {
		return
	}
	select {
	case r.ch <- ev:
	default:
		if n := r.dropped.Add(1); n == 1 {
			// Warn once; the counter carries the rest.
			slog.Warn("audit log queue full, dropping events", "path", r.path)
		}
	}
}

// Single goroutine, so neither the file nor the ring needs write-side locking
// beyond the ring's own (readers come from the UI thread).
func (r *AuditRecorder) run() {
	defer close(r.done)
	enc := json.NewEncoder(r.w)
	for {
		select {
		case ev, ok := <-r.ch:
			if !ok {
				return // Close() closed the queue; buffered events already drained
			}
			r.write(enc, ev)
		case req := <-r.syncCh:
			req.reply <- r.writeDurable(enc, req.ev)
		case ack := <-r.flushCh:
			// select gives no ordering guarantee between the two channels, so
			// drain everything already queued before acknowledging: by the
			// time a flush request lands, every event enqueued before it is
			// sitting in r.ch.
			for {
				select {
				case ev := <-r.ch:
					r.write(enc, ev)
					continue
				default:
				}
				break
			}
			close(ack)
		}
	}
}

func (r *AuditRecorder) write(enc *json.Encoder, ev AuditEvent) {
	r.ring.add(ev)
	if err := enc.Encode(ev); err != nil {
		slog.Warn("audit log write failed", "error", err)
	}
	r.wrote.Add(1)

	r.sinkMu.RLock()
	sink := r.sink
	r.sinkMu.RUnlock()
	if sink != nil {
		sink(ev)
	}
}

// Unlike write, the ring and the live UI sink are updated only *after* the
// bytes are down: a record relay refused to stand behind must not show up in
// the Tool Calls tab as though it had been logged.
func (r *AuditRecorder) writeDurable(enc *json.Encoder, ev AuditEvent) error {
	if err := enc.Encode(ev); err != nil {
		slog.Warn("audit durable write failed", "path", r.path, "error", err)
		return fmt.Errorf("audit write: %w", err)
	}
	if err := syncAuditWriter(r.w); err != nil {
		slog.Warn("audit durable sync failed", "path", r.path, "error", err)
		return fmt.Errorf("audit sync: %w", err)
	}
	r.ring.add(ev)
	r.wrote.Add(1)

	r.sinkMu.RLock()
	sink := r.sink
	r.sinkMu.RUnlock()
	if sink != nil {
		sink(ev)
	}
	return nil
}

// rotatingWriter implements this; an in-memory writer used by a test may not,
// and a sink with no notion of stable storage is not something to refuse a
// tool call over — the bytes still reached it.
type auditSyncer interface{ Sync() error }

func syncAuditWriter(w io.Writer) error {
	s, ok := w.(auditSyncer)
	if !ok {
		return nil
	}
	return s.Sync()
}

// Safe to call more than once.
func (r *AuditRecorder) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		close(r.ch)
		<-r.done
		_ = r.w.Close()
	})
}

// CloseWriterForTest closes the underlying log file out from under the
// writer goroutine, so a test can make a write fail for real (an unwritable
// disk, say) rather than through a test-only switch in production code. It
// deliberately does not go through Close, which also tears down the writer
// goroutine itself.
//
// This is a test seam, not a production capability — see
// config.NewSettingsStoreWithCache for the same pattern. It panics outside a
// test binary.
func (r *AuditRecorder) CloseWriterForTest() error {
	if !testing.Testing() {
		panic("audit: CloseWriterForTest is a test seam and must not be reached in a shipped binary")
	}
	return r.w.Close()
}

func (r *AuditRecorder) Wrote() uint64 {
	if r == nil {
		return 0
	}
	return r.wrote.Load()
}

// Mainly a test seam, but also used before an export so the file on disk
// includes everything the UI has already shown. Returns immediately if the
// recorder is closed rather than blocking forever on a dead writer.
func (r *AuditRecorder) Flush() {
	if r == nil {
		return
	}
	ack := make(chan struct{})
	select {
	case r.flushCh <- ack:
	case <-r.done:
		return
	}
	select {
	case <-ack:
	case <-r.done:
	}
}

// ScopeViolation is a field, not a stored outcome (ADR-011 decision 7), but is
// accepted as a value of AuditQuery.Outcome anyway: it is what a security
// review reaches for first, right beside "denied". matches() special-cases it
// rather than storing a taxonomy in ScopeViolation's own outcome.
const auditOutcomeScopeViolation = "scope_violation"

// Empty fields don't filter.
type AuditQuery struct {
	ProjectID string `json:"project_id,omitempty"`
	McpID     string `json:"mcp_id,omitempty"`
	// Matches ev.Outcome verbatim, EXCEPT "scope_violation" (see
	// auditOutcomeScopeViolation), which instead matches any record with
	// ev.ScopeViolation set, whatever its actual outcome.
	Outcome string `json:"outcome,omitempty"`
	Event   string `json:"event,omitempty"`
	// Filters on the actor kind, which is how "everything any VM did"
	// (kind=remote) is asked as one question.
	Kind  string `json:"kind,omitempty"`
	Text  string `json:"text,omitempty"` // substring over tool, args, error, project name
	Limit int    `json:"limit,omitempty"`
	// Searches the log file rather than the in-memory ring, for history older
	// than the ring holds. Bounded by AuditTailBudget.
	Deep bool `json:"deep,omitempty"`
}

func (q AuditQuery) Matches(ev *AuditEvent) bool {
	if q.ProjectID != "" && ev.Actor.ProjectID != q.ProjectID {
		return false
	}
	if q.McpID != "" && ev.McpID != q.McpID {
		return false
	}
	if q.Outcome == auditOutcomeScopeViolation {
		if !ev.ScopeViolation {
			return false
		}
	} else if q.Outcome != "" && ev.Outcome != q.Outcome {
		return false
	}
	if q.Event != "" && ev.Event != q.Event {
		return false
	}
	if q.Kind != "" && ev.Actor.Kind != q.Kind {
		return false
	}
	if q.Text != "" {
		needle := strings.ToLower(q.Text)
		hay := strings.ToLower(strings.Join([]string{
			ev.Tool, ev.McpID, ev.Error, ev.Actor.ProjectName,
			ev.Actor.Proc, ev.Actor.Parent, string(ev.Args),
			ev.Method, ev.Path, ev.Class, ev.Transport, ev.Actor.CredID,
			ev.Credential, ev.Subject, ev.SubjectName, ev.Via, strings.Join(ev.Grants, ","),
		}, "\x00"))
		if !strings.Contains(hay, needle) {
			return false
		}
	}
	return true
}

// Reads the in-memory ring unless q.Deep is set, in which case it scans the
// tail of the log file.
func (r *AuditRecorder) Query(q AuditQuery) []AuditEvent {
	if r == nil {
		return []AuditEvent{}
	}
	limit := intOr(q.Limit, 200)

	var candidates []AuditEvent
	if q.Deep {
		candidates = ReadAuditTail(r.path, AuditTailBudget)
	} else {
		candidates = r.ring.snapshot()
	}

	out := make([]AuditEvent, 0, limit)
	for i := range candidates {
		if q.Matches(&candidates[i]) {
			out = append(out, candidates[i])
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

// Reads at most budget bytes from the end of the JSONL log and returns the
// events newest-first. Unparseable lines are skipped rather than failing the
// whole query — a truncated tail should still be readable.
func ReadAuditTail(path string, budget int64) []AuditEvent {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil
	}
	start := int64(0)
	partial := false
	if info.Size() > budget {
		start = info.Size() - budget
		partial = true
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil
	}

	scanner := newAuditScanner(f)
	var events []AuditEvent
	first := true
	for scanner.Scan() {
		if first && partial {
			first = false
			continue // discard the record the seek cut in half
		}
		first = false
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev AuditEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		events = append(events, ev)
	}

	// Reverse into newest-first order to match the ring's contract.
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	return events
}

// LogPath is the on-disk path of the tool-call audit log, given the directory
// relay's rotated logs live under (cmd/relay's serviceLogDir).
func LogPath(logDir string) string {
	return filepath.Join(logDir, "audit", "toolcalls.jsonl")
}

// StartAuditRecorder is the tray's constructor. Auditing is observability,
// not an authorization control, so a broken sink degrades to "no audit log"
// rather than taking relay down — the Tool Calls tab makes the disabled state
// visible instead of pretending.
//
// logDir and openWriter are the two things this package does not own: where
// relay's rotated logs live, and how a log file rotates (shared by relay's
// own log and every managed service's log, cmd/relay/log_rotate.go).
func StartAuditRecorder(cfg *config.AuditConfig, logDir string, openWriter OpenWriter) *AuditRecorder {
	path := LogPath(logDir)
	rec, err := NewAuditRecorder(cfg, path, openWriter)
	if err != nil {
		slog.Error("audit log disabled", "path", path, "error", err)
		return nil
	}
	if rec == nil {
		slog.Info("tool-call audit log disabled by settings")
		return nil
	}
	slog.Info("tool-call audit log started", "path", path,
		"log_args", rec.cfg.LogArgs, "log_lists", rec.cfg.LogLists,
		"max_file_bytes", rec.cfg.MaxFileBytes, "generations", rec.cfg.Generations)
	return rec
}

// The buffer is sized for the largest record the caps allow (redacted args
// plus an optional result preview) with generous headroom, so an oversized
// line is skipped rather than truncating the rest of the scan.
func newAuditScanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 4<<20)
	return s
}

// Separate function so tests can reason about it without reaching for uuid
// directly.
func NewAuditID() string { return uuid.NewString() }
