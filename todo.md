# Relay TODO

- [ ] Investigate: `relay service register` cannot connect to bridge socket (`relay.sock`) even though Relay tray app is running. The `ReloadService` request fails with "no such file or directory" on the Unix socket. Possibly the socket path changed, the socket isn't being created, or there's a permissions issue. (Discovered 2026-03-26)
- [ ] Investigate: Relay tray app shutdown stalls — quitting the app hangs instead of cleanly exiting. May be a service shutdown ordering issue, a blocking goroutine, or a hung cgo/Cocoa call. SIGTERM was ignored by all three processes (Relay, relayScheduler, relayLLM) — required SIGKILL to terminate. (Discovered 2026-03-26) — **ROOT CAUSE IDENTIFIED, see CR-1 below.**
- [ ] Eve: Scheduled task session view doesn't update to show completion. Sidebar correctly shows task finished (no spinner) but the main chat window still shows "Running Write..." spinner. Requires a page refresh to see the completed state. Likely the `task_completed` WebSocket event updates the sidebar task status but doesn't trigger a re-render of the active session's message stream. (Discovered 2026-03-26)

---

# Code Review — 2026-06-13

Reviewer notes: every finding below was read at the cited line and reasoned
through against the surrounding code. Claims raised during review that did NOT
hold up are listed under "Verified clean / debunked" so they aren't re-raised.

## Confirmed Issues

### Critical

**CR-1 — Bridge shutdown hangs (likely the documented SIGKILL-required stall)**
- **bridge/server.go:94-142** (`handleConn`) **+ trayapp.go:470-485** (shutdown order)
- **Severity:** critical
- **Issue:** `handleConn` loops on `scanner.Scan()` with no read deadline and never watches its per-connection `ctx.Done()`, so `StopAccepting()` (which only cancels `s.ctx` and closes the *listener*) cannot unblock an in-flight handler; `Close()`→`wg.Wait()` then blocks forever on any handler stuck in `Scan()`. This is compounded by ordering: `bs.Close()` (trayapp.go:475) runs *before* `registry.StopAll()` (trayapp.go:485), so if a managed service (relayLLM/relayScheduler) holds a persistent bridge connection, `Close()` waits for it to disconnect while the process that would disconnect it isn't killed until later — a circular wait that matches the "SIGTERM ignored by all three processes" symptom.
- **Fix:** In `handleConn`, after deriving `ctx`, add `go func() { <-ctx.Done(); _ = conn.Close() }()` so server-context cancellation closes the socket and unblocks `Scan()`. Independently, reorder shutdown in trayapp.go so `a.registry.StopAll()` runs before `a.bridgeServer.Close()` (kill the talkers before draining the listener). Optionally add a per-request idle read deadline reset inside the scan loop.

### Major

**CR-2 — OAuth discovery is a blind SSRF surface (no scheme/host validation, redirects followed)**
- **oauth.go:165-218** (`discoverOAuth`), client at **oauth.go:62**
- **Severity:** major
- **Issue:** `resourceMetaURL` (taken from the MCP server's `WWW-Authenticate` header) and `authServerBase = prm.AuthorizationServers[0]` (from the fetched PRM doc) are passed straight to `oauthHTTPClient.Get(...)` with no scheme/host validation, and `oauthHTTPClient` follows redirects by default. A malicious or compromised (admin-registered) MCP can make relay issue GETs to arbitrary URLs (localhost services, link-local) and derive its token endpoint from an attacker-chosen host.
- **Fix:** Run `resourceMetaURL`, `authServerBase`, and the constructed well-known URLs through `validateMcpURL` (https/http + non-empty host); reject `http://` for non-loopback hosts and ideally constrain `authServerBase`'s host to the MCP's registrable domain. Set `oauthHTTPClient.CheckRedirect` to reject or re-validate redirect targets.

**CR-3 — HTTP MCP tool calls have no request timeout at runtime**
- **http_mcp.go:205-263** (`SendRequest`) / **newHTTPMcpConn http_mcp.go:83-91** vs **external_mcp.go:753** (stdio's `MCPRequestTimeout` timer)
- **Severity:** major
- **Issue:** stdio `SendRequest` bounds each call with `MCPRequestTimeout`; the HTTP path relies solely on the passed `ctx`, which at runtime (`CallTool`) carries no deadline (only startup wraps `MCPStartupTimeout`). The client also uses `http.DefaultTransport` (no `ResponseHeaderTimeout`) and deliberately sets no client `Timeout`. A hung/malicious HTTP MCP can block a tool call (and its bridge handler goroutine) indefinitely.
- **Fix:** Give `httpMcpConn.httpClient` a transport with `ResponseHeaderTimeout` (bounds time-to-first-byte without killing SSE bodies), and/or wrap runtime `SendRequest` with `context.WithTimeout(ctx, MCPRequestTimeout)`.

**CR-4 — OAuth refresh storm when the token response omits `expires_in`**
- **http_mcp.go:149-151** (`applyRefreshedToken`) **+ http_mcp.go:122-138** (`tokenRefreshSnapshot`)
- **Severity:** major
- **Issue:** When a refresh response has `ExpiresIn <= 0`, `tokenExpiry` is left at its old (now-past) value. `tokenRefreshSnapshot` then sees `!IsZero() && now.After(expiry-window)` → `needsRefresh == true` on *every* subsequent request, triggering a token refresh on each call (added latency + possible token-endpoint rate-limiting/lockout).
- **Fix:** When `tokenResp.ExpiresIn <= 0`, set `c.oauth.tokenExpiry = time.Time{}` (zero disables proactive refresh) or apply a sane default TTL, instead of leaving the stale timestamp.

**CR-5 — Status poller leaks a transport (idle conns/FDs) every tick**
- **service_status_poller.go:59** (`NewServiceStatusClient` per service per tick) **+ enhanced_services.go:48-54** (`newUnixHTTPTransport`)
- **Severity:** major
- **Issue:** Each poll tick constructs a fresh `http.Client` with a fresh `http.Transport` per service. The transport is never closed and sets no `IdleConnTimeout`, so a keep-alive idle Unix-socket connection (plus its read-loop goroutine + FD) lingers per service per tick until GC — a steady leak under the ~30/min poll cadence.
- **Fix:** Cache one `*ServiceStatusClient` per service (e.g. alongside `EnhancedService.proxy`), or set `IdleConnTimeout`/`ResponseHeaderTimeout` on `newUnixHTTPTransport`, or `defer client.http.CloseIdleConnections()` after each fetch.

**CR-6 — WebSocket proxy has no read deadline / ping-pong → half-open leaks**
- **frontend_dispatcher.go:113-134** (`forwardDispatchedWS`)
- **Severity:** major
- **Issue:** The two pump goroutines block on `src.NextReader()` with no read deadline, pong handler, or ping loop. A peer that dies without a close frame (network partition, killed process) never unblocks the pump, leaking both `*websocket.Conn`s and the goroutines. (Partly mitigated because both ends are local Unix sockets, which usually error on peer exit — but true half-open still leaks.)
- **Fix:** Set an idle `SetReadDeadline` extended by a `SetPongHandler`, and run a periodic ping writer tied to `closeBoth` on failure.

**CR-7 — Model-allowlist guard is bound to an exact path; sibling/trailing-slash variants bypass it**
- **frontend_server.go:66** (`mux.Handle("POST /api/sessions", ...)`) **+ frontend_model_guard.go:36-70**
- **Severity:** major
- **Issue:** The allowlist (relay's only enforcement point for `allowed_models`) is mounted on the exact pattern `POST /api/sessions`. Go's `ServeMux` sends `POST /api/sessions/` (trailing slash) or any sibling create path to the `mux.Handle("/", dispatcher)` catch-all with no enforcement. If relayLLM treats a trailing-slash variant as session-create, the model allowlist is bypassed.
- **Fix:** Don't gate a policy boundary on a single exact mux pattern. Wrap the dispatcher so the guard inspects every `POST` whose path is `"/api/sessions"` or `strings.HasPrefix(path, "/api/sessions/")` before forwarding.

**CR-8 — Project create/update orchestration duplicated between HTTP and IPC**
- **project_routes.go:160-268** (PUT) vs **ipc_projects.go:115-211** (`ipcUpdateProject`); also **project_routes.go:100-158** (POST) vs **ipc_projects.go:56-111** (`ipcCreateProject`)
- **Severity:** major (maintainability)
- **Issue:** Although the leaf `Settings.UpdateProject*` mutators are shared, the *orchestration* is copy-pasted: pointer-field dispatch, the `needsSchemas` computation, the "empty PermissionPolicy = clear" rule (project_routes.go:224 vs ipc_projects.go:164), and the entire disabled-tools diff loop (project_routes.go:233-248 vs ipc_projects.go:172-189). ~50 lines duplicated verbatim; any new patchable field must be edited in two places or they silently diverge.
- **Fix:** Extract transport-agnostic `applyProjectCreate(s *Settings, fields)` / `applyProjectUpdate(s *Settings, id, fields) (Project, bool)` helpers operating on a shared field struct; call from both the `store.With` closure and the `withSettings` closure. Callers keep only their response/event differences.

### Minor

**CR-9 — Dead code: `DoResource`**
- **service_status_client.go:65-70** — `DoResource` (and its resource-CRUD doc) is a leftover from the removed `resources[]` feature; grep confirms no callers. **Fix:** delete it.

**CR-10 — `progressTokenString` redundant `float64` case**
- **external_mcp.go:44-47** — `case float64` and `default` are identical (`fmt.Sprintf("%v", t)`). **Fix:** drop the float case (or use `strconv.FormatFloat(t,'f',-1,64)` if canonical numeric form is ever needed).

**CR-11 — `CallTool` double-unmarshals non-object `_meta`**
- **external_mcp.go:598-602** — re-unmarshals `meta` into `raw` (already attempted at :570) and discards the error; `json.Valid` was already checked at :571. **Fix:** decode once into `interface{}` up front and branch on whether it's a `map`.

**CR-12 — `generateRandomHex` panics the tray on RNG failure**
- **service_registry.go:228-235** — `panic` on `rand.Read` error takes down the whole tray. **Fix:** return `(string, error)` and propagate from `Start`; reuse `generateProjectToken`'s helper.

**CR-13 — Manifest action `Method` validated upper-cased but stored verbatim**
- **bridge/manifest.go:206** — `Validate` checks `strings.ToUpper(a.Method)` but stores the original case; `ipc_service_action.go:70` uses `action.Method` verbatim in the HTTP request, so a manifest declaring `"get"` issues a literal `get` verb that most servers won't match. **Fix:** normalize in place: `m.Actions[i].Method = strings.ToUpper(a.Method)` during validation.

**CR-14 — Oversized bridge line is dropped silently**
- **bridge/server.go:139-141** — when a line exceeds `MaxMessageSize`, `scanner.Err()` is `bufio.ErrTooLong`; it's logged at Debug and the connection is dropped with no error frame, and the same error surfaces on the client (bridge/client.go:227-229) as a generic "read failed". **Fix:** detect `errors.Is(err, bufio.ErrTooLong)`, emit a `CodeInvalidParams` error frame before closing, and log non-EOF read errors at Warn.

**CR-15 — Streaming bridge deadline is a fixed cap, not idle-based**
- **bridge/client.go:199** — `conn.SetDeadline(now+bridgeTimeout)` is set once and never extended; a tool call that legitimately streams progress for >10 min is killed mid-stream even while actively producing output. **Fix:** reset the deadline on each received frame inside the `scanner.Scan()` loop so 10 min becomes an inactivity timeout.

**CR-16 — Reload/reconcile bridge handlers always report success**
- **bridge/server.go:211-224** — `handleReloadService`/`handleReconcile`/`handleReloadMcp` ignore the (void) router results and always return `RespOK`, so a CLI `relay service restart` of a missing/failed service reports success. **Fix:** have `ReloadService`/`ReloadExternalMcp` return an error and surface it via `bridgeError(classifyErrorCode(err), ...)`.

**CR-17 — IPC create validates PermissionPolicy inside the write closure + rolls back**
- **ipc_projects.go:75-83** — `ipcCreateProject` validates the policy *after* `CreateProjectWithToken` runs and rolls back via `RemoveProject`, whereas the HTTP POST validates before mutating (project_routes.go:115). The rollback is fragile and asymmetric. **Fix:** hoist `validatePermissionPolicy(msg.PermissionPolicy)` above `withSettings` and delete the rollback branch.

**CR-18 — `resolveConfigPath`: weak root when `WorkingDir` unset + residual stat→open TOCTOU**
- **service_config_file.go:46-48** and **:53-75 / :81-95** — when `allowedRoot` is empty it defaults to `filepath.Dir(decl.Path)`, so the containment check only catches a symlink at the *final* component, not a parent-component symlink (the root is silently as wide as wherever the parent resolves). Separately, `readConfigFile`/`writeConfigFile` re-open the resolved path by string, so a swap between `resolveConfigPath` and open isn't fully closed. Low impact (service-token authenticated, same-user, defense-in-depth). **Fix:** require an explicit `WorkingDir` (fail closed) instead of synthesizing a root; have `resolveConfigPath` return the `os.FileInfo` and re-verify via `os.SameFile` on the opened fd (or open with `O_NOFOLLOW`).

**CR-19 — HTTP MCP stores an unbounded session ID from an untrusted server**
- **http_mcp.go:241-245** — `c.sessionID = sid` is taken verbatim from the remote `Mcp-Session-Id` response header and echoed on every subsequent request; CR/LF is rejected by net/http but length is unbounded. **Fix:** reject `len(sid) > 1024` before storing.

**CR-20 — `parseSSEResponse` has no event-count cap**
- **http_mcp.go:299-325** — per-event `maxDataSize` bounds memory but not stream duration; a server streaming endless non-matching events keeps the (now-untimed, see CR-3) call alive. **Fix:** add a bounded counter of parsed events without a match and fail fast. (Largely subsumed once CR-3 adds a request timeout.)

## Possible Risks (lower confidence / context-dependent)

- **mcp/server.go:51-59** — `tools/call` is dispatched to unbounded goroutines (`wg.Add(1); go handleToolsCall`) with no concurrency cap; each holds a fresh bridge Unix connection. The client is a single LLM-side MCP client (local trust), so real-world risk is low, but there's no backpressure. *Fix:* gate with a buffered semaphore.
- **http_mcp.go:164-191** (`refreshTokenIfNeeded`) — a transient refresh failure hard-fails the MCP call even when the current access token is still valid (refresh fires 30s early). *Fix:* if `time.Now().Before(tokenExpiry)`, log and proceed with the existing token; only hard-fail once actually expired.
- **service_registry.go:111-118 + service_registry_unix.go:61** — `RELAY_SERVICE_TOKEN` (full bridge access) is placed in the service's env and the service is spawned via a login shell (`-l`), so the token is inherited by every grandchild unless the service scrubs it, and startup depends on user dotfiles + unvalidated `$SHELL`. This is the documented model (the service genuinely needs the token), but `EnvServiceTokenLegacy` doubles the exposure. *Fix:* drop the legacy duplicate now; document the scrub-before-spawn requirement; consider validating `$SHELL`.
- **oauth.go:319-345** — the callback handler can be invoked more than once (browser retry); `codeCh` has capacity 1, so a second success send blocks that handler goroutine until process exit. *Fix:* guard delivery with `sync.Once` or a non-blocking select-send.
- **service_registry.go:188-216** — the reaper does not delete the exited entry from `r.processes`; it's only reaped lazily by `isRunningLocked`/`CleanupDead` (in practice soon, because `OnProcessExit` triggers a `RunningIDs` refresh). Low impact. *Fix:* eagerly `delete` in the reaper guarded by `if r.processes[id] == proc`.
- **ipc_services.go** (reported by sub-agent, not line-verified here) — `ipcUpdateServiceAutostart` persists without a UI refresh (inconsistent with siblings); `ipcStartService` silently no-ops on an unknown ID. *Fix:* emit a refresh/error event in both.

## Nice-to-Have Improvements

- **enhanced_services.go:48-54** — add `ResponseHeaderTimeout` + `IdleConnTimeout` to `newUnixHTTPTransport`; benefits both the reverse proxy (wedged-service protection) and the status client (also mitigates CR-5).
- **gofmt drift** (pre-commit runs build/vet/test but not gofmt): tokens.go var-block alignment, external_mcp.go:50-51 double blank line, settings_store.go trailing blanks. *Fix:* add `gofmt -l` to `.githooks` and reformat.
- **bridge/client.go:150-160** — `sendAdmin` sets `Token:` explicitly even though `NewClient(token)` already holds it (two sources of truth). *Fix:* use `c.token`.
- **helpers.go `mergeEnv`** — duplicate-key override works only by Go's last-wins exec semantics over a randomly-ordered map; make the merge explicit via a `map[string]string` keyed overlay.

## Verified clean / debunked (raised during review, did NOT hold)

- **Reaper/`Stop` pidfile race (alleged critical):** FALSE. Defers run LIFO and `defer close(proc.done)` is registered (service_registry.go:194) *before* `removePidFile`/token-remove/`Forget` (:198-207), so those cleanups complete *before* `done` closes; `Stop`'s `<-proc.done` therefore unblocks only after pidfile removal. No replacement-pidfile deletion.
- **Service internal token leaking to the settings WebView via the status batch:** FALSE. `ServiceStatusSnapshot.Manifest` is `bridge.Manifest`, which does not carry `InternalToken`/`InternalSocket` — those are separate fields on `EnhancedService`/`RegisterManifestRequest` (verified bridge/manifest.go:15-36).
- **Missing `validateProjectPath` on the create paths:** FALSE. `CreateProjectWithToken` calls `validateProjectPath` internally (project.go:22), covering both HTTP POST and IPC create.
- **Frontend bearer auth fail-open on empty token:** CLEAN. `frontendBearerAuth` (frontend_server.go:131-137) rejects all requests when the configured token is empty; constant-time compare at :147.
- **Project token leakage in HTTP responses:** CLEAN. `projectView`/`projectsToView` strip the token; `rotate_token` (project_routes.go:333) is the sole token-returning route, as documented.
- **`externalMcpConn` unbounded progress goroutines:** the unbounded `go fn(params)` branch (external_mcp.go:184-189) is only reachable for test/mock conns; production conns from `spawnStdioConn` always set `progressSem` (a 64-deep semaphore), so production delivery is bounded.

---

## Summary

Overall the codebase is in good health: the security-critical paths (project-token
brokering, constant-time auth, fail-closed bearer checks, token stripping in DTOs,
the action-template escaping, and the config-path gate) are carefully built and
well-documented, and the concurrency primitives (lock ordering, write-mutex
separation, idempotent `Close`) show real thought. The defects that remain cluster
in **lifecycle/shutdown** and **I/O hygiene on external boundaries** (timeouts,
connection cleanup, SSRF) rather than in core auth logic, and several "scary"
candidate findings turned out to be already handled. The most leverage is in three
actions: **(1)** fix the bridge shutdown hang (make `handleConn` close its socket on
context cancellation and kill services before draining the listener — CR-1, which
resolves a standing production bug); **(2)** harden the OAuth discovery boundary
against SSRF (validate discovery URLs, disable open redirects — CR-2); and **(3)**
close the external-I/O resource/timeout gaps as one pass (HTTP MCP runtime timeout
CR-3, refresh-storm CR-4, status-poller transport leak CR-5, WS deadlines CR-6,
plus `ResponseHeaderTimeout`/`IdleConnTimeout` on the shared Unix transport).

---

## Resolution — 2026-06-13 (branch claude/sharp-mendel-cebs5x)

All 20 confirmed issues (CR-1 … CR-20) fixed, plus several Possible-Risk and
Nice-to-Have items. Build + `go vet` + the hermetic suite (`go test ./...`) pass;
concurrency-sensitive fixes (CR-1, CR-6, CR-15) also pass under `-race`. New or
extended regression tests accompany every fix except where noted.

Fixed (with tests):
- CR-1  bridge shutdown hang — `handleConn` now closes its conn on ctx cancel
        (`TestServer_CloseDoesNotHangOnIdleConnection`). NOTE: kept the existing
        `cleanup()` ordering (registry.StopAll runs after bridgeServer.Close) —
        its orphan-prevention rationale is documented and the socket-close fix
        resolves the hang regardless of order, so the review's reorder was not
        applied.
- CR-2  OAuth SSRF — `validateOAuthDiscoveryURL` gate on every discovery/token
        fetch + redirect re-validation (`CheckRedirect`); callback channel sends
        made non-blocking. (`TestValidateOAuthDiscoveryURL`, `TestFetch*SSRF*`,
        `TestOAuthClient_RejectsRedirectToBlockedTarget`).
- CR-3  HTTP MCP runtime timeout — `SendRequest` wraps ctx with MCPRequestTimeout
        (`TestHTTPMcpConn_SendRequest_TimesOutHungServer`).
- CR-4  refresh storm — clear stale expiry when `expires_in` absent
        (`TestHTTPMcpConn_RefreshWithoutExpiresIn_DisablesProactiveRefresh`).
- CR-5  status-poller transport leak — `CloseIdleConnections` after each fetch +
        `IdleConnTimeout` on the shared Unix transport.
- CR-6  WS half-open leak — read deadline + pong handler + periodic pinger
        (existing `TestFrontendDispatcher_WSNoGoroutineLeak` covers teardown).
- CR-7  model-allowlist bypass — guard is now the catch-all wrapper, gating the
        create path + trailing-slash variant only (`*_TrailingSlash`,
        `*_IgnoresSubResourcePath`, `*_IgnoresNonPost`).
- CR-8  project create/update orchestration deduped into `applyProjectCreate` /
        `applyProjectUpdate` over shared field structs (project_apply.go).
- CR-9  deleted dead `DoResource`.
- CR-10 simplified `progressTokenString`.
- CR-11 `CallTool` decodes `_meta` once.
- CR-12 `generateRandomHex` returns an error instead of panicking; propagated
        through generateProjectToken / CreateProjectWithToken / RotateProjectToken
        / service + frontend token mint.
- CR-13 manifest action `Method` normalized to upper-case in `Validate`
        (`TestManifestValidate_NormalizesActionMethodCase`).
- CR-14 oversized bridge line emits an InvalidParams error frame
        (`TestServer_OversizedLineGetsErrorFrame`).
- CR-15 streaming bridge deadline is now idle-based
        (`TestContract_StreamingResetsIdleDeadline`).
- CR-16 ReloadService / ReloadExternalMcp surface errors over the bridge
        (`TestContract_Reload*_SurfacesError`).
- CR-17 IPC create validates the policy before the write closure (rollback gone).
- CR-18 config-path TOCTOU: `resolveConfigPath` returns FileInfo, `readConfigFile`
        re-verifies via `os.SameFile` (`TestReadConfigFile_RejectsSwappedFile`).
        NOTE: did NOT make `WorkingDir` mandatory (point 1) — that fail-closed
        change could break the editor for services without a workdir; left as
        documented defense-in-depth.
- CR-19 oversized `Mcp-Session-Id` rejected (`*_RejectsOversizedSessionID`).
- CR-20 SSE event-count cap added (request timeout from CR-3 is the primary bound).

Also done: refresh-failure-while-token-still-valid now proceeds (possible risk);
`sendAdmin` uses `c.token`; `external_mcp.go` double blank line removed; gofmt on
touched files.

Deferred (deliberately not changed this pass):
- mcp/server.go unbounded tools/call goroutines — low real-world risk (single
  local LLM client); semaphore not added.
- `EnvServiceTokenLegacy` duplicate — still needed for un-migrated relayLLM;
  dropping is a migration-timing call.
- reaper eager-delete; ipc_services autostart-refresh / start-no-op — minor,
  not line-verified.
- `gofmt -l` in the pre-commit hook and reformatting pre-existing drift in
  untouched files — out of scope to keep this diff focused.
