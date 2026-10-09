package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"

	"github.com/barelyworkingcode/relay/internal/logging"
)

// The remote verbs are the CLI door to the remote-listener settings: the same
// EnrolmentOps methods GET and PUT /api/remote call. The gate (remote.configure)
// lives in SetRemoteConfig.

// adminRemoteView is remote.view. A read, so it writes the event the route
// writes.
func adminRemoteView(ctx context.Context, r *appRouter, _ json.RawMessage) (_ json.RawMessage, err error) {
	ev := logging.BeginEvent(ctx, "remote.config.get")
	defer func() { endEvent(ev, err) }()
	ops, err := requireEnrolmentOps(r)
	if err != nil {
		return nil, err
	}
	return marshalAdminResult(ops.RemoteConfig())
}

func adminRemoteSet(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	ops, err := requireEnrolmentOps(r)
	if err != nil {
		return nil, err
	}
	body, err := decodeAdminArgs[remoteConfigFields]("remote.set", args)
	if err != nil {
		return nil, err
	}
	view, err := ops.SetRemoteConfig(ctx, body, auditViaCLI, "")
	if err != nil {
		return nil, err
	}
	return marshalAdminResult(view)
}

func printRemoteLine(raw json.RawMessage) {
	var v remoteConfigView
	decodeCLIResult(raw, &v)
	state := "off"
	if v.Enabled {
		state = "on"
	}
	enrol := "off"
	if v.EnrolmentRequests {
		enrol = "on"
	}
	fmt.Printf("remote listener %s at %s; enrolment requests %s at %s\n", state, v.Effective, enrol, v.EnrolmentEffective)
}

func remoteShow(args []string) {
	fs := flag.NewFlagSet("remote show", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the remote configuration as JSON")
	fs.Parse(args)

	raw := adminCall("relay remote show", "remote.view", nil)
	if *asJSON {
		printJSONLine(raw)
		return
	}
	printRemoteLine(raw)
}

// remoteSet replaces the whole remote record with a PUT /api/remote body, so
// the fields a body leaves out are cleared, as they are on the route.
func remoteSet(args []string) {
	fs := flag.NewFlagSet("remote set", flag.ExitOnError)
	file := fs.String("file", "", "PUT /api/remote body as a JSON file, or - for stdin (required)")
	asJSON := fs.Bool("json", false, "print the new remote configuration as JSON")
	fs.Parse(args)
	if *file == "" {
		exitError("--file is required")
	}

	raw := adminCall("relay remote set", "remote.set", json.RawMessage(readBodyArg(*file)))
	if *asJSON {
		printJSONLine(raw)
		return
	}
	printRemoteLine(raw)
}
