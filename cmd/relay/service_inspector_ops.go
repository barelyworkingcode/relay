package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/service"
)

// The inspector cores are the one place a service's declared actions and config
// file are checked and used; the Settings IPC handlers and the CLI verbs both
// call them, so neither door decides what the manifest permits.

const serviceActionTimeout = 10 * time.Second

// runServiceAction runs one action the service's manifest declares. The
// manifest is the whitelist: an action it does not declare is refused, and the
// path comes from the manifest, never from the caller.
func runServiceAction(ctx context.Context, enhanced *EnhancedServiceRegistry, serviceID, actionID string, row map[string]json.RawMessage) (err error) {
	ev := logging.BeginEvent(ctx, "service.action")
	defer func() {
		ev.Set("service_id", serviceID).Set("action_id", actionID)
		endEvent(ev, err)
	}()
	if enhanced == nil {
		return invalidService("no enhanced registry")
	}
	rec := enhanced.Get(serviceID)
	if rec == nil {
		return notFoundf("service %q not registered", serviceID)
	}
	action := findAction(rec.Manifest.Actions, actionID)
	if action == nil {
		return markInvalid(fmt.Errorf("action %q not declared by service %q", actionID, serviceID))
	}
	path, err := buildActionPath(action, row)
	if err != nil {
		return markInvalid(err)
	}

	client := service.NewStatusClient(rec.InternalSocket, rec.InternalToken)
	defer client.CloseIdleConnections()
	callCtx, cancel := context.WithTimeout(ctx, serviceActionTimeout)
	defer cancel()
	if _, err := client.DoAction(callCtx, action.Method, path); err != nil {
		slog.Warn("service action failed", "service", serviceID, "action", actionID, "error", err)
		return upstreamErr(err)
	}
	return nil
}

// readServiceConfig reads the config file a service's manifest declares. The
// path resolves through service.ResolveConfigPath, the single containment gate.
func readServiceConfig(ctx context.Context, store config.SettingsStore, enhanced *EnhancedServiceRegistry, serviceID string) (text string, err error) {
	ev := logging.BeginEvent(ctx, "service.config.get")
	defer func() {
		ev.Set("service_id", serviceID)
		endEvent(ev, err)
	}()
	if enhanced == nil {
		return "", invalidService("no enhanced registry")
	}
	rec := enhanced.Get(serviceID)
	if rec == nil {
		return "", notFoundf("service %q not registered", serviceID)
	}
	decl := rec.Manifest.Config
	if decl == nil {
		return "", invalidService(fmt.Sprintf("service %q declares no config file", serviceID))
	}
	allowedRoot := ""
	if svc, _ := config.FindServiceByID(config.FreshSettings(store), serviceID); svc != nil {
		allowedRoot = svc.WorkingDir
	}
	realPath, info, err := service.ResolveConfigPath(decl, allowedRoot)
	if err != nil {
		return "", err
	}
	data, err := service.ReadConfigFile(realPath, info)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
