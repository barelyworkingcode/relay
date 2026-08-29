package main

import (
	"context"
	"encoding/json"
)

// adminOpHandler executes one brokered admin operation. args is the
// caller's request payload, forwarded unmodified; the result is opaque JSON,
// the same shape CallTool already hands back for a result it does not
// itself interpret.
type adminOpHandler func(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error)

// adminOps is the inner dispatch table behind bridge.ReqAdminOp (ADR-017
// decision 2). Empty here: nothing registers into it yet, so admin_op
// resolves no operation at all and appRouter.AdminOp refuses everything.
var adminOps = map[string]adminOpHandler{}
