package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/enrolment"
	"github.com/barelyworkingcode/relay/internal/mcpbroker"
	"os"
	"path/filepath"
	"time"
)

// adminOpHandler executes one brokered admin operation. args is the
// caller's request payload, forwarded unmodified; the result is opaque JSON,
// the same shape CallTool already hands back for a result it does not
// itself interpret.
type adminOpHandler func(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error)

// adminOps is the inner dispatch table behind bridge.ReqAdminOp (ADR-017
// implementation spec §7.1/§7.2). Every entry dispatches into the S5 core
// that carries the presence gate — never into a free function or a store
// mutator directly, which is exactly the shortcut this table exists to
// remove (§7.2's operation table is normative for the op name -> core
// mapping).
var adminOps = map[string]adminOpHandler{
	"credential.mint":           adminCredentialMint,
	"credential.revoke":         adminCredentialRevoke,
	"enrolment.create":          adminEnrolmentCreate,
	"enrolment.sign":            adminEnrolmentSign,
	"enrolment.update":          adminEnrolmentUpdate,
	"enrolment.revoke":          adminEnrolmentRevoke,
	"enrolment.request.list":    adminEnrolmentRequestList,
	"enrolment.request.approve": adminEnrolmentRequestApprove,
	"enrolment.request.refuse":  adminEnrolmentRequestRefuse,
	"login.bootstrap.mint":      adminLoginBootstrapMint,
	"login.passkey.revoke":      adminLoginPasskeyRevoke,
	"mcp.register":              adminMcpRegister,
	"mcp.unregister":            adminMcpUnregister,
	"service.register":          adminServiceRegister,
	"service.unregister":        adminServiceUnregister,
	"service.restart":           adminServiceRestart,
}

// decodeAdminArgs unmarshals an admin_op payload into T, naming the
// operation in the error so a malformed CLI request refuses legibly rather
// than as a bare "unexpected end of JSON input".
func decodeAdminArgs[T any](op string, args json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(args, &v); err != nil {
		var zero T
		return zero, fmt.Errorf("%s: invalid request: %w", op, err)
	}
	return v, nil
}

func marshalAdminResult(v any) (json.RawMessage, error) {
	return json.Marshal(v)
}

// requireCredentialOps and its siblings below are the one place each admin
// op checks that the tray actually wired the core it needs. A production
// relay always has (trayapp.go constructs all six at startup); a test
// appRouter that forgot one gets a named refusal instead of a nil-pointer
// panic three calls deep inside the core.
func requireCredentialOps(r *appRouter) (*CredentialOps, error) {
	if r.credentialOps == nil {
		return nil, errors.New("credential operations are not available in this relay process")
	}
	return r.credentialOps, nil
}

func requireEnrolmentOps(r *appRouter) (*EnrolmentOps, error) {
	if r.enrolmentOps == nil {
		return nil, errors.New("enrolment operations are not available in this relay process")
	}
	return r.enrolmentOps, nil
}

func requireLoginOps(r *appRouter) (*LoginOps, error) {
	if r.loginOps == nil {
		return nil, errLoginOpsUnavailable
	}
	return r.loginOps, nil
}

func requireMcpOps(r *appRouter) (*McpOps, error) {
	if r.mcpOps == nil {
		return nil, errors.New("mcp operations are not available in this relay process")
	}
	return r.mcpOps, nil
}

func requireServiceOps(r *appRouter) (*ServiceOps, error) {
	if r.serviceOps == nil {
		return nil, errors.New("service operations are not available in this relay process")
	}
	return r.serviceOps, nil
}

// credentialMintResult and credentialRevokeRequest are admin_op's own wire
// types: APICredential already marshals safely (its Hash field is a
// verifier, never the token), so the mint response is just the record plus
// the one plaintext moment alongside it.
type credentialMintResult struct {
	Credential config.APICredential `json:"credential"`
	Token      string               `json:"token"`
}

type credentialRevokeRequest struct {
	ID string `json:"id"`
}

func adminCredentialMint(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireCredentialOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[credentialMintRequest]("credential.mint", args)
	if err != nil {
		return nil, err
	}
	cred, token, err := ops.Mint(ctx, req, auditViaCLI, "")
	if err != nil {
		return nil, err
	}
	return marshalAdminResult(credentialMintResult{Credential: cred, Token: token})
}

func adminCredentialRevoke(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireCredentialOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[credentialRevokeRequest]("credential.revoke", args)
	if err != nil {
		return nil, err
	}
	removed, err := ops.Revoke(ctx, req.ID, auditViaCLI, "")
	if err != nil {
		return nil, err
	}
	return marshalAdminResult(removed)
}

// enrolmentCreateResult carries what enrolCreate needs to print: the record
// (never the client private key, which stays on disk in Dir — the same
// withholding rule EnrolmentCreated and enrolmentBundleView already apply
// to the IPC door) and, when the settings write landed but the bundle write
// to disk failed, the reason so the CLI can say so rather than claiming a
// clean success.
type enrolmentCreateResult struct {
	Enrolment   config.Enrolment `json:"enrolment"`
	Dir         string           `json:"dir,omitempty"`
	BundleError string           `json:"bundle_error,omitempty"`
}

func adminEnrolmentCreate(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireEnrolmentOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[enrolmentFields]("enrolment.create", args)
	if err != nil {
		return nil, err
	}
	created, err := ops.Create(ctx, req, auditViaCLI, "")
	if err != nil && !errors.Is(err, enrolment.ErrBundle) {
		return nil, err
	}
	result := enrolmentCreateResult{Enrolment: created.Enrolment, Dir: created.Dir}
	if err != nil {
		result.BundleError = err.Error()
	}
	return marshalAdminResult(result)
}

// enrolmentSignResult carries what enrolSign needs to print and, when
// --out was given, write: the record, and the certificate bytes
// themselves. Certificates are public; there is no field here, and none is
// to be added, that could carry a private key.
type enrolmentSignResult struct {
	Enrolment   config.Enrolment `json:"enrolment"`
	Dir         string           `json:"dir,omitempty"`
	CertPEM     string           `json:"cert_pem"`
	CAPEM       string           `json:"ca_pem"`
	BundleError string           `json:"bundle_error,omitempty"`
	// RequestExpired is set only by adminEnrolmentRequestApprove (Sign has
	// no pending row to expire): the row was swept between lodge and
	// approve, but the enrolment above committed anyway. enrolApprove must
	// say so plainly rather than claim the client will collect a row that
	// no longer exists.
	RequestExpired bool `json:"request_expired,omitempty"`
	// RequestRefused is RequestExpired's sibling, set when the row was
	// instead found refused: the operator declined this exact request
	// while THIS approval's presence prompt was still open. Kept as its
	// own field rather than folded into RequestExpired -- the two rows
	// differ in every fact enrolApprove's note is built from (issue #93).
	RequestRefused bool `json:"request_refused,omitempty"`
}

func adminEnrolmentSign(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireEnrolmentOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[enrolmentSignFields]("enrolment.sign", args)
	if err != nil {
		return nil, err
	}
	created, err := ops.Sign(ctx, req, auditViaCLI, "")
	if err != nil && !errors.Is(err, enrolment.ErrBundle) {
		return nil, err
	}
	result := enrolmentSignResult{Enrolment: created.Enrolment, Dir: created.Dir}
	if err != nil {
		result.BundleError = err.Error()
		return marshalAdminResult(result)
	}
	// The bundle landed: read the certificates back off disk (public
	// files, readable by the same process that just wrote them) so the CLI
	// can print and optionally copy them without ever touching the config
	// dir itself.
	certPEM, err := os.ReadFile(filepath.Join(created.Dir, "client.crt"))
	if err != nil {
		return nil, fmt.Errorf("enrolment.sign: read issued certificate: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Join(created.Dir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("enrolment.sign: read ca certificate: %w", err)
	}
	result.CertPEM = string(certPEM)
	result.CAPEM = string(caPEM)
	return marshalAdminResult(result)
}

type enrolmentUpdateResult struct {
	Before config.Enrolment `json:"before"`
	After  config.Enrolment `json:"after"`
}

func adminEnrolmentUpdate(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireEnrolmentOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[enrolment.UpdateRequest]("enrolment.update", args)
	if err != nil {
		return nil, err
	}
	before, after, err := ops.Update(ctx, req, auditViaCLI, "")
	if err != nil {
		return nil, err
	}
	return marshalAdminResult(enrolmentUpdateResult{Before: before, After: after})
}

type enrolmentRevokeRequest struct {
	ClientID string `json:"client_id"`
}

func adminEnrolmentRevoke(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireEnrolmentOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[enrolmentRevokeRequest]("enrolment.revoke", args)
	if err != nil {
		return nil, err
	}
	removed, err := ops.Revoke(ctx, req.ClientID, auditViaCLI, "")
	if err != nil {
		return nil, err
	}
	return marshalAdminResult(removed)
}

// enrolmentRequestListItem is the CLI-facing projection of
// enrolmentRequestView: JSON tags for `relay enrol requests --json`, and a
// timestamp format (RFC3339) rather than time.Time's own, matching every
// other admin_op result that carries one.
type enrolmentRequestListItem struct {
	RequestID        string `json:"request_id"`
	SPKISHA256       string `json:"spki_sha256"`
	Label            string `json:"label,omitempty"`
	RemoteAddr       string `json:"remote_addr"`
	ArrivedAt        string `json:"arrived_at"`
	ExpiresAt        string `json:"expires_at"`
	Approved         bool   `json:"approved"`
	ApprovedClientID string `json:"approved_client_id,omitempty"`

	// The comparison code and its states, so `relay enrol requests` renders
	// the same three answers the Settings panel does — see
	// enrolRequestSASColumn. IsLegacyRequest marks a `relayremote request`
	// row, which has no comparison and never gains one.
	SAS              string `json:"sas,omitempty"`
	SASReady         bool   `json:"sas_ready"`
	SASFailed        bool   `json:"sas_failed"`
	IsLegacyRequest  bool   `json:"is_legacy_request"`
	RequestedProfile string `json:"requested_profile,omitempty"`
}

type enrolmentRequestListResult struct {
	Requests []enrolmentRequestListItem `json:"requests"`
}

func adminEnrolmentRequestList(_ context.Context, r *appRouter, _ json.RawMessage) (json.RawMessage, error) {
	ops, err := requireEnrolmentOps(r)
	if err != nil {
		return nil, err
	}
	views := ops.PendingRequests()
	items := make([]enrolmentRequestListItem, 0, len(views))
	for _, v := range views {
		items = append(items, enrolmentRequestListItem{
			RequestID:        v.RequestID,
			SPKISHA256:       v.SPKISHA256,
			Label:            v.Label,
			RemoteAddr:       v.RemoteAddr,
			ArrivedAt:        v.ArrivedAt.UTC().Format(time.RFC3339),
			ExpiresAt:        v.ExpiresAt.UTC().Format(time.RFC3339),
			Approved:         v.Approved,
			ApprovedClientID: v.ApprovedClientID,
			SAS:              v.SAS,
			SASReady:         v.SASReady,
			SASFailed:        v.SASFailed,
			IsLegacyRequest:  v.IsLegacyRequest,
			RequestedProfile: v.RequestedProfile,
		})
	}
	return marshalAdminResult(enrolmentRequestListResult{Requests: items})
}

// adminEnrolmentRequestApprove answers with enrolmentSignResult — the exact
// shape adminEnrolmentSign already returns (Enrolment, Dir, the certificate
// bytes, and BundleError when enrolment.WriteSignedCertBundle failed) — because
// approving is enrolment.sign's second door, not a different result shape.
func adminEnrolmentRequestApprove(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireEnrolmentOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[approveFields]("enrolment.request.approve", args)
	if err != nil {
		return nil, err
	}
	created, err := ops.Approve(ctx, req, auditViaCLI, "")
	if err != nil && !errors.Is(err, enrolment.ErrBundle) && !errors.Is(err, errEnrolmentRequestExpired) && !errors.Is(err, errEnrolmentRequestRefused) {
		return nil, err
	}
	result := enrolmentSignResult{Enrolment: created.Enrolment, Dir: created.Dir, CertPEM: created.CertPEM, CAPEM: created.CAPEM}
	if errors.Is(err, enrolment.ErrBundle) {
		result.BundleError = err.Error()
	}
	if errors.Is(err, errEnrolmentRequestExpired) {
		result.RequestExpired = true
	}
	if errors.Is(err, errEnrolmentRequestRefused) {
		result.RequestRefused = true
	}
	return marshalAdminResult(result)
}

type enrolmentRequestRefuseRequest struct {
	RequestID string `json:"request_id"`
}

func adminEnrolmentRequestRefuse(_ context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireEnrolmentOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[enrolmentRequestRefuseRequest]("enrolment.request.refuse", args)
	if err != nil {
		return nil, err
	}
	if err := ops.Refuse(req.RequestID); err != nil {
		return nil, err
	}
	return marshalAdminResult(struct {
		RequestID string `json:"request_id"`
	}{RequestID: req.RequestID})
}

func adminLoginBootstrapMint(ctx context.Context, r *appRouter, _ json.RawMessage) (json.RawMessage, error) {
	ops, err := requireLoginOps(r)
	if err != nil {
		return nil, err
	}
	view, err := ops.MintBootstrap(ctx, auditViaCLI)
	if err != nil {
		return nil, err
	}
	return marshalAdminResult(view)
}

type loginPasskeyRevokeRequest struct {
	ID string `json:"id"`
}

func adminLoginPasskeyRevoke(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireLoginOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[loginPasskeyRevokeRequest]("login.passkey.revoke", args)
	if err != nil {
		return nil, err
	}
	removed, err := ops.RevokePasskey(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	return marshalAdminResult(removed)
}

// adminMcpRegister responds with mcpView, the same JSON-safe projection the
// HTTP and IPC doors already use (Env revealed for display, OAuthState
// withheld entirely). mcpbroker.ErrAuthRequired is folded into AuthRequired
// rather than treated as a failure: the record landed even though
// McpOps.Add returned a non-nil error, so this is a success from admin_op's
// point of view and the CLI reports it as one.
func adminMcpRegister(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireMcpOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[mcpFields]("mcp.register", args)
	if err != nil {
		return nil, err
	}
	result, err := ops.Add(ctx, req, auditViaCLI, "")
	if err != nil && !errors.Is(err, mcpbroker.ErrAuthRequired) {
		return nil, err
	}
	view := mcpViewOf(result)
	if errors.Is(err, mcpbroker.ErrAuthRequired) {
		view.AuthRequired = true
	}
	return marshalAdminResult(view)
}

type mcpUnregisterRequest struct {
	ID string `json:"id"`
}

func adminMcpUnregister(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireMcpOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[mcpUnregisterRequest]("mcp.unregister", args)
	if err != nil {
		return nil, err
	}
	if err := ops.Remove(ctx, req.ID, auditViaCLI, ""); err != nil {
		return nil, err
	}
	return marshalAdminResult(struct {
		ID string `json:"id"`
	}{ID: req.ID})
}

// adminServiceRegister dispatches to Update when the id already exists and
// Create otherwise, matching how HTTP's two routes (POST /api/services,
// PUT /api/services/{id}) already split this: `relay service register` has
// always been the one command that covers both, so this is where that
// convenience lives now that a single core backs every door. The id is
// req.resolvedID() — the caller's explicit --id when given, slugify(name)
// otherwise — so an operator re-registering under a stable id they chose
// finds the same record Create originally wrote, not a second one under
// whatever --name slugifies to today. ServiceOps.Update itself decides what
// "fewer flags than the first time" means: a field the request leaves nil
// (a CLI flag not repeated) carries the stored value forward unchanged, and
// only a field the request actually sets is applied — including to its
// zero value, so `--autostart=false` still turns autostart off.
func adminServiceRegister(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireServiceOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[serviceFields]("service.register", args)
	if err != nil {
		return nil, err
	}
	id := req.resolvedID()
	var cfg config.ServiceConfig
	var opErr error
	if _, getErr := ops.Get(id); getErr == nil {
		cfg, opErr = ops.Update(ctx, id, req, auditViaCLI, "")
	} else {
		cfg, opErr = ops.Create(ctx, req, auditViaCLI, "")
	}
	if opErr != nil && !errors.Is(opErr, errServiceProcess) {
		return nil, opErr
	}
	view := serviceViewOf(cfg, ops.Registry.IsRunning(cfg.ID))
	view = withProcessError(view, opErr)
	return marshalAdminResult(view)
}

type serviceUnregisterRequest struct {
	ID string `json:"id"`
}

func adminServiceUnregister(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireServiceOps(r)
	if err != nil {
		return nil, err
	}
	req, err := decodeAdminArgs[serviceUnregisterRequest]("service.unregister", args)
	if err != nil {
		return nil, err
	}
	if err := ops.Remove(ctx, req.ID, auditViaCLI, ""); err != nil {
		return nil, err
	}
	return marshalAdminResult(struct {
		ID string `json:"id"`
	}{ID: req.ID})
}

// service.restart is not gated (§6.4: it changes no settings, it restarts
// what is already configured) and needs no ServiceOps method of its own —
// appRouter.ReloadService is the exact Stop-then-Start-in-place the tray
// has always done for this, reused rather than duplicated.
type serviceRestartRequest struct {
	ID string `json:"id"`
}

func adminServiceRestart(_ context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	req, err := decodeAdminArgs[serviceRestartRequest]("service.restart", args)
	if err != nil {
		return nil, err
	}
	if err := r.ReloadService(req.ID); err != nil {
		return nil, err
	}
	return marshalAdminResult(struct {
		ID string `json:"id"`
	}{ID: req.ID})
}
