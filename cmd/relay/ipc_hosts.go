package main

import "encoding/json"

// Host IPC handlers for relay's native Hosts tab. These mirror
// host_routes.go (the HTTP surface eve uses) but emit events instead of
// returning HTTP bodies, exactly as ipc_projects.go mirrors project_routes.go
// — both paths share HostOps, so a host created over IPC is identical to one
// created over HTTP.

type ipcHostIDMsg struct {
	ID string `json:"id"`
}

// ipcUpdateHostMsg carries the host id inline, unlike the HTTP PUT route.
type ipcUpdateHostMsg struct {
	ID string `json:"id"`
	hostPatchFields
}

func ipcListHosts(ctx *IPCContext, raw json.RawMessage) {
	if ctx.HostOps == nil {
		ctx.UI.EmitEvent("onHostsListed", marshalForUI(hostsToView(nil)))
		return
	}
	ctx.UI.EmitEvent("onHostsListed", marshalForUI(hostsToView(ctx.HostOps.List())))
}

// ipcCreateHost runs off the main thread: Create probes the host over ssh,
// which is a real network round trip capped at 30s (docs/ssh-hosts.md) and
// must not freeze the Settings window for that long.
func ipcCreateHost(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[hostFields](raw, "create_host")
	if !ok {
		return
	}
	ctx.GoFunc(func() {
		created, err := ctx.HostOps.Create(ctx.Ctx, *msg)
		if err != nil {
			dispatchEmit(ctx, "onHostError", err.Error())
			return
		}
		dispatchEmit(ctx, "onHostAdded", marshalForUI(hostToView(created)))
	})
}

// ipcUpdateHost runs off the main thread for the same reason as
// ipcCreateHost: a target/port/identity_file change triggers a synchronous
// re-probe.
func ipcUpdateHost(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcUpdateHostMsg](raw, "update_host")
	if !ok || msg.ID == "" {
		return
	}
	ctx.GoFunc(func() {
		updated, found, err := ctx.HostOps.Update(ctx.Ctx, msg.ID, msg.hostPatchFields)
		if err != nil {
			dispatchEmit(ctx, "onHostError", err.Error())
			return
		}
		if !found {
			dispatchEmit(ctx, "onHostError", "host not found")
			return
		}
		dispatchEmit(ctx, "onHostUpdated", marshalForUI(hostToView(updated)))
	})
}

func ipcRemoveHost(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcHostIDMsg](raw, "remove_host")
	if !ok || msg.ID == "" {
		return
	}
	found, refs, err := ctx.HostOps.Remove(msg.ID)
	if err != nil {
		ctx.UI.EmitEvent("onHostError", err.Error())
		return
	}
	if !found {
		ctx.UI.EmitEvent("onHostError", "host not found")
		return
	}
	if len(refs) > 0 {
		ctx.UI.EmitEvent("onHostError", "host is used by: "+joinNames(refs))
		return
	}
	ctx.UI.EmitEvent("onHostRemoved", msg.ID)
}

// ipcProbeHost is the explicit "Probe" button — runs off the main thread,
// same reasoning as ipcCreateHost.
func ipcProbeHost(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcHostIDMsg](raw, "probe_host")
	if !ok || msg.ID == "" {
		return
	}
	ctx.GoFunc(func() {
		updated, found, err := ctx.HostOps.Probe(ctx.Ctx, msg.ID)
		if err != nil {
			dispatchEmit(ctx, "onHostError", err.Error())
			return
		}
		if !found {
			dispatchEmit(ctx, "onHostError", "host not found")
			return
		}
		dispatchEmit(ctx, "onHostUpdated", marshalForUI(hostToView(updated)))
	})
}

func ipcDisconnectHost(ctx *IPCContext, raw json.RawMessage) {
	msg, ok := unmarshalIPC[ipcHostIDMsg](raw, "disconnect_host")
	if !ok || msg.ID == "" {
		return
	}
	ctx.GoFunc(func() {
		updated, found, err := ctx.HostOps.Disconnect(msg.ID)
		if err != nil {
			dispatchEmit(ctx, "onHostError", err.Error())
			return
		}
		if !found {
			dispatchEmit(ctx, "onHostError", "host not found")
			return
		}
		dispatchEmit(ctx, "onHostUpdated", marshalForUI(hostToView(updated)))
	})
}

func joinNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
