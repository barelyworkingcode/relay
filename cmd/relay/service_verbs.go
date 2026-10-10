package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"

	"github.com/barelyworkingcode/relay/internal/logging"
)

// The service verbs below are the CLI door to ServiceOps (start, stop, save a
// config file) and to the inspector cores the Settings Services tab calls.
// The manifest stays the authority for actions and config files.

type serviceActionRequest struct {
	ServiceID string                     `json:"service_id"`
	ActionID  string                     `json:"action_id"`
	Row       map[string]json.RawMessage `json:"row,omitempty"`
}

type serviceActionResult struct {
	ServiceID string `json:"service_id"`
	ActionID  string `json:"action_id"`
	OK        bool   `json:"ok"`
}

type serviceConfigRequest struct {
	ServiceID string `json:"service_id"`
	Text      string `json:"text,omitempty"`
}

type serviceConfigReadResult struct {
	ServiceID string `json:"service_id"`
	Text      string `json:"text"`
}

type serviceConfigSaveResult struct {
	ServiceID string `json:"service_id"`
	Restarted bool   `json:"restarted"`
}

func adminServiceStart(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	return adminServiceLifecycle(ctx, r, "service.start", args, (*ServiceOps).Start)
}

func adminServiceStop(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	return adminServiceLifecycle(ctx, r, "service.stop", args, (*ServiceOps).Stop)
}

func adminServiceLifecycle(ctx context.Context, r *appRouter, op string, args json.RawMessage, act func(*ServiceOps, context.Context, string) error) (json.RawMessage, error) {
	req, err := decodeAdminArgs[serviceRestartRequest](op, args)
	if err != nil {
		return nil, err
	}
	ops, err := requireServiceOps(r)
	if err != nil {
		return nil, err
	}
	id, err := resolveServiceRef(r, req.ID, req.Name)
	if err != nil {
		endEvent(logging.BeginEvent(ctx, op), err)
		return nil, err
	}
	if err := act(ops, ctx, id); err != nil {
		return nil, err
	}
	return marshalAdminResult(struct {
		ID string `json:"id"`
	}{ID: id})
}

func adminServiceAction(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	req, err := decodeAdminArgs[serviceActionRequest]("service.action", args)
	if err != nil {
		return nil, err
	}
	if err := runServiceAction(ctx, r.enhanced, req.ServiceID, req.ActionID, req.Row); err != nil {
		return nil, err
	}
	return marshalAdminResult(serviceActionResult{ServiceID: req.ServiceID, ActionID: req.ActionID, OK: true})
}

func adminServiceConfigGet(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	req, err := decodeAdminArgs[serviceConfigRequest]("service.config.get", args)
	if err != nil {
		return nil, err
	}
	text, err := readServiceConfig(ctx, r.store, r.enhanced, req.ServiceID)
	if err != nil {
		return nil, err
	}
	return marshalAdminResult(serviceConfigReadResult{ServiceID: req.ServiceID, Text: text})
}

func adminServiceConfigSave(ctx context.Context, r *appRouter, args json.RawMessage) (json.RawMessage, error) {
	req, err := decodeAdminArgs[serviceConfigRequest]("service.config.save", args)
	if err != nil {
		return nil, err
	}
	ops, err := requireServiceOps(r)
	if err != nil {
		return nil, err
	}
	res, err := ops.SaveConfigFile(ctx, req.ServiceID, req.Text)
	if errors.Is(err, errServiceProcess) {
		// The file landed and only the restart failed; the caller must not
		// read this as an unsaved file.
		return nil, fmt.Errorf("config saved, but the service did not restart: %w", err)
	}
	if err != nil {
		return nil, err
	}
	return marshalAdminResult(serviceConfigSaveResult{ServiceID: req.ServiceID, Restarted: res.Restarted})
}

// serviceLifecycleVerb runs `service start` and `service stop`, which differ
// only in the op and the word they print.
func serviceLifecycleVerb(name, op, past string, args []string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	id := fs.String("id", "", "service ID")
	svcName := fs.String("name", "", "service display name")
	asJSON := fs.Bool("json", false, `print {"id"} as JSON`)
	fs.Parse(args)
	if *id == "" && *svcName == "" {
		exitError("--id or --name is required")
	}

	raw := adminCall("relay "+name, op, serviceRestartRequest{ID: *id, Name: *svcName})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var res serviceRestartRequest
	decodeCLIResult(raw, &res)
	fmt.Printf("%s service %q\n", past, res.ID)
}

func serviceStart(args []string) {
	serviceLifecycleVerb("service start", "service.start", "started", args)
}
func serviceStop(args []string) {
	serviceLifecycleVerb("service stop", "service.stop", "stopped", args)
}

// serviceAction runs an action the service's manifest declares.
func serviceAction(args []string) {
	fs := flag.NewFlagSet("service action", flag.ExitOnError)
	id := fs.String("id", "", "service ID (required)")
	action := fs.String("action", "", "action ID the service's manifest declares (required)")
	row := fs.String("row", "", `row values as a JSON object, for an action that acts on a row`)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	fs.Parse(args)
	if *id == "" || *action == "" {
		exitError("--id and --action are required")
	}
	req := serviceActionRequest{ServiceID: *id, ActionID: *action}
	if *row != "" {
		if err := json.Unmarshal([]byte(*row), &req.Row); err != nil {
			exitError("--row must be a JSON object: %v", err)
		}
	}

	raw := adminCall("relay service action", "service.action", req)
	if *asJSON {
		printJSONLine(raw)
		return
	}
	fmt.Printf("ran action %q on service %q\n", *action, *id)
}

// serviceConfig reads a service's config file, or with --set replaces it and
// restarts the service.
func serviceConfig(args []string) {
	fs := flag.NewFlagSet("service config", flag.ExitOnError)
	id := fs.String("id", "", "service ID (required)")
	set := fs.String("set", "", "replace the config file with this file's text, or - for stdin")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	fs.Parse(args)
	if *id == "" {
		exitError("--id is required")
	}

	if *set == "" {
		raw := adminCall("relay service config", "service.config.get", serviceConfigRequest{ServiceID: *id})
		if *asJSON {
			printJSONLine(raw)
			return
		}
		var res serviceConfigReadResult
		decodeCLIResult(raw, &res)
		fmt.Print(res.Text)
		return
	}

	raw := adminCall("relay service config", "service.config.save", serviceConfigRequest{ServiceID: *id, Text: string(readBodyArg(*set))})
	if *asJSON {
		printJSONLine(raw)
		return
	}
	var res serviceConfigSaveResult
	decodeCLIResult(raw, &res)
	if res.Restarted {
		fmt.Printf("saved the config of service %q and restarted it\n", res.ServiceID)
		return
	}
	fmt.Printf("saved the config of service %q\n", res.ServiceID)
}
