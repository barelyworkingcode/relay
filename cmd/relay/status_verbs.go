package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"

	"github.com/barelyworkingcode/relay/internal/logging"
)

var errStatusUnavailable = errors.New("the overview is not available in this relay process")

// statusView is what the Overview tab shows, as one document: the version,
// the sealed-store warning, the two Reveal folders, and the live state of
// every MCP and service.
type statusView struct {
	Version            string                                  `json:"version"`
	SealStatus         string                                  `json:"seal_status"` // "" when healthy
	Paths              pathsView                               `json:"paths"`
	MCPHealth          map[string]nativeMcpHealthView          `json:"mcp_health"`
	ServiceRuntime     map[string]nativeServiceRuntimeView     `json:"service_runtime"`
	ServiceSupervision map[string]nativeServiceSupervisionView `json:"service_supervision"`
}

// adminStatusView is status.view. A read, so its event is written here: the
// seed it reads is also built by the Settings window.
func adminStatusView(ctx context.Context, r *appRouter, _ json.RawMessage) (_ json.RawMessage, err error) {
	ev := logging.BeginEvent(ctx, "status.view")
	defer func() { endEvent(ev, err) }()
	if r.overview == nil || r.serviceOps == nil || r.serviceOps.Registry == nil {
		return nil, errStatusUnavailable
	}
	seed := r.overview()
	return marshalAdminResult(statusView{
		Version:            seed.Version,
		SealStatus:         seed.SealStatus,
		Paths:              seed.Paths,
		MCPHealth:          seed.MCPHealth,
		ServiceRuntime:     seed.ServiceRuntime,
		ServiceSupervision: serviceSupervisionToNativeView(r.serviceOps.Registry.SupervisionStatuses()),
	})
}

const statusUsage = "Usage: relay [--config-dir DIR] status [--json]"

func runStatusCommand(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the status document as one line of JSON")
	fs.Parse(args)
	if fs.NArg() > 0 {
		exitError("unexpected argument %q\n%s", fs.Arg(0), statusUsage)
	}

	raw := adminCall("relay status", "status.view", nil)
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var v statusView
	decodeCLIResult(raw, &v)
	seal := "sealed store ok"
	if v.SealStatus != "" {
		seal = "sealed store degraded: " + v.SealStatus
	}
	fmt.Printf("relay %s: %d MCP(s), %d service(s) running, %s\n", v.Version, len(v.MCPHealth), len(v.ServiceRuntime), seal)
}
