package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
)

var errHostOpsUnavailable = errors.New("host operations are not available in this relay process")

type hostIDRequest struct {
	ID string `json:"id"`
}

// The host verbs are the CLI door to HostOps.Probe and HostOps.Disconnect,
// the cores the Hosts tab and the /api/hosts action routes call; the events
// live in the cores.

func adminHostAction(op string, act func(ops *HostOps, ctx context.Context, id string) (hostView, bool, error)) adminOpHandler {
	return func(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
		if r.hostOps == nil {
			return nil, errHostOpsUnavailable
		}
		req, err := decodeAdminArgs[hostIDRequest](op, args)
		if err != nil {
			return nil, err
		}
		view, found, err := act(r.hostOps, ctx, req.ID)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errHostNotFound
		}
		return marshalAdminResult(view)
	}
}

var (
	adminHostProbe = adminHostAction("host.probe", func(ops *HostOps, ctx context.Context, id string) (hostView, bool, error) {
		h, found, err := ops.Probe(ctx, id)
		return hostToView(h), found, err
	})
	adminHostDisconnect = adminHostAction("host.disconnect", func(ops *HostOps, ctx context.Context, id string) (hostView, bool, error) {
		h, found, err := ops.Disconnect(ctx, id)
		return hostToView(h), found, err
	})
)

func hostVerb(name, op, past string, args []string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	id := fs.String("id", "", "host id (required)")
	asJSON := fs.Bool("json", false, "print the host as JSON")
	fs.Parse(args)
	if *id == "" {
		exitError("--id is required")
	}

	raw := adminCall("relay "+name, op, hostIDRequest{ID: *id})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var h hostView
	decodeCLIResult(raw, &h)
	fmt.Printf("%s host %s (%s): %s\n", past, h.Name, h.ID, h.Status)
}

func hostProbe(args []string) { hostVerb("host probe", "host.probe", "probed", args) }
func hostDisconnect(args []string) {
	hostVerb("host disconnect", "host.disconnect", "disconnected", args)
}
