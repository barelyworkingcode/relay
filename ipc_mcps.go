package main

import (
	"encoding/json"
	"errors"
)

// Must run from background goroutines only: WKWebView's evaluateJavaScript
// crashes the process if called off the main thread.
func dispatchEmit(ctx *IPCContext, event string, args ...interface{}) {
	ctx.Platform.DispatchToMain(func() {
		ctx.UI.EmitEvent(event, args...)
	})
}

// Despite the name, the events emitted here are not always errors.
func dispatchError(ctx *IPCContext, event string, args ...interface{}) {
	dispatchEmit(ctx, event, args...)
}

func (msg *ipcAddExternalMcpMsg) fields() mcpFields {
	return mcpFields{
		DisplayName: msg.DisplayName,
		Transport:   msg.Transport,
		URL:         msg.URL,
		Command:     msg.Command,
		Args:        msg.Args,
		Env:         msg.Env,
		TccServices: msg.TccServices,
	}
}

func ipcAddExternalMcp(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcAddExternalMcpMsg](raw, "add_external_mcp")
	if !ok {
		return
	}
	fields := msg.fields()

	// Validation now lives in McpOps.Add, so this fires unconditionally
	// rather than only once the request is known-legal; the request is
	// about to run through Add regardless, and the spinner starting a beat
	// before an "invalid" toast is a cosmetic cost, not a behavior change.
	ctx.UI.EmitEvent("onDiscoveryStarted")

	// Off the main thread: Add spawns a stdio child or does an HTTP
	// handshake, either of which can block for the length of
	// MCPDiscoveryTimeout.
	ctx.GoFunc(func() {
		result, err := ctx.McpOps.Add(ctx.Ctx, fields, auditViaIPC, "")
		ctx.Platform.DispatchToMain(func() {
			if err != nil && !errors.Is(err, ErrAuthRequired) {
				ctx.UI.EmitEvent("onExternalMcpError", err.Error())
				return
			}
			ctx.UI.EmitEvent("onExternalMcpAdded", marshalForUI(externalMcpToNativeView(result)))
			if errors.Is(err, ErrAuthRequired) {
				ctx.UI.EmitEvent("onOAuthRequired", result.ID)
			}
		})
	})
}

func ipcAuthenticateMcp(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "authenticate_mcp")
	if !ok || msg.ID == "" {
		return
	}

	// Off the main thread: StartOAuth blocks on OAuth discovery, the local
	// callback listener, and the token exchange.
	ctx.GoFunc(func() {
		dispatchEmit(ctx, "onOAuthStarted", msg.ID)

		// ctx.Platform.OpenURL is the one desktop dependency in this whole
		// flow; McpOps.StartOAuth takes it as a parameter precisely so this
		// is the only place it gets supplied (ADR-014 section 4).
		if _, err := ctx.McpOps.StartOAuth(ctx.Ctx, msg.ID, ctx.Platform.OpenURL, auditViaIPC, ""); err != nil {
			dispatchError(ctx, "onOAuthError", msg.ID, err.Error())
			return
		}
		dispatchEmit(ctx, "onOAuthComplete", msg.ID)
	})
}

func ipcRemoveExternalMcp(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcIDMsg](raw, "remove_external_mcp")
	if !ok || msg.ID == "" {
		return
	}

	// Off the main thread: McpOps.Remove is gated (mcp.unregister, §6.4 of
	// the ADR-017 implementation spec), and Gate.Require blocks on
	// LocalAuthentication's async completion handler, which needs the
	// Cocoa run loop pumped to be delivered — the same deadlock
	// showLoginCode's doc comment in trayapp.go describes.
	ctx.GoFunc(func() {
		if err := ctx.McpOps.Remove(ctx.Ctx, msg.ID, auditViaIPC, ""); err != nil {
			dispatchEmit(ctx, "onExternalMcpError", err.Error())
			return
		}
		dispatchEmit(ctx, "onExternalMcpRemoved", msg.ID)
	})
}
