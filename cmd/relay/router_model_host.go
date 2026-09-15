package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/jsonrpc"
	"github.com/barelyworkingcode/relay/internal/service"
)

// RegisterModelHost authenticates the caller's launch identity, which must
// hold the model_host capability, then hands the registration to
// r.modelHosts. Own service id only, the same rule RegisterManifest applies:
// a manifest or a model host registered under any other id would outlive
// the process that serves it.
func (r *appRouter) RegisterModelHost(ctx context.Context, req bridge.RegisterModelHostRequest, token string) error {
	id, err := r.requireServiceIdentity(ctx, token, service.OpRegisterModelHost)
	if err != nil {
		return err
	}
	if req.ServiceID != id.Name {
		return jsonrpc.NewCodedError(jsonrpc.CodeUnauthorized, fmt.Errorf("%s: service %q may not register a model host for %q", bridge.ReqRegisterModelHost, id.Name, req.ServiceID))
	}
	if r.modelHosts == nil {
		return jsonrpc.NewCodedError(jsonrpc.CodeInternalError, fmt.Errorf("model host registry is not wired"))
	}
	if err := r.modelHosts.Register(req.ServiceID, req.RouterSocket, id.Process); err != nil {
		return jsonrpc.NewCodedError(jsonrpc.CodeInvalidParams, err)
	}
	slog.Info("model host registered", "service", req.ServiceID, "socket", req.RouterSocket)
	return nil
}
