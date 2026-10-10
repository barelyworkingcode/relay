# Map proof

Each row is one catalogued break (`e2e/coverage/proof/breaks.json`) that turned at least one
selected feature test red. `go run ./coverage/cmd/mapproof --repo .. --all --record ../docs/map-proof.md`
(in `e2e/`) reruns the proof; a run with `--areas` updates only those rows. It selects tests with
the same code the `e2e` job uses and leaves out `TestFeatureMapCoverage`, which checks the map
and not the area. Rule M5 fails when an area has no break, no row here, or (if `journey-only`)
no listing below.

| Area | File | Break | Base | Red tests | Date |
|---|---|---|---|---|---|
| audit | `internal/audit/audit.go` | the audit project filter keeps the other projects' rows | ed78c986dd6c | TestAuditCLIFilters, TestAuditQueryHTTP, TestSessionLaunchRows | 2026-10-10 |
| credentials | `internal/control/control.go` | a credential of the wrong class answers 401 instead of 403 | ed78c986dd6c | TestClassEnforcement, TestCredentialMint | 2026-10-10 |
| doors | `cmd/relay/doors.go` | a gated door is listed as not owner-gated | ed78c986dd6c | TestDoorsNameClassAndGates | 2026-10-10 |
| files | `cmd/relay/file_routes.go` | a missing file answers 400 instead of 404 | ed78c986dd6c | TestFileChangeOps, TestFileStream | 2026-10-10 |
| grants | `cmd/relay/router.go` | the disabled-tool lookup swaps key and value, so a disabled tool is still granted | ed78c986dd6c | TestDisabledToolRefused | 2026-10-10 |
| hosts | `cmd/relay/host_routes.go` | an unknown host answers 400 instead of 404 | ed78c986dd6c | TestHostListAndGet | 2026-10-10 |
| instance | `cmd/relay/clock_relaytest.go` | advancing the instance clock moves it backwards | ed78c986dd6c | TestClockMovesCredentialExpiry | 2026-10-10 |
| logging | `internal/logging/logread.go` | the logs trace filter keeps the other traces' lines | ed78c986dd6c | TestBug296SandboxedSessionAdminOpsRefused, TestCredentialListHidesToken, TestCredentialMint, TestCredentialMintDenied, TestCredentialRevoke (+5 more) | 2026-10-10 |
| login | `internal/ceremonylimit/ceremonylimit.go` | the ceremony back-off never holds a caller off | ed78c986dd6c | TestBrowserSessionsListAndSignOut, TestPasskeyList, TestPasskeyRegisterAndSignIn, TestPasskeyRegisterBadCodeRefused, TestPasskeyRevoke (+2 more) | 2026-10-10 |
| models | `internal/modelbroker/normalise.go` | the model grant check is inverted: an ungranted model is allowed | ed78c986dd6c | TestModelEndpointRoutes, TestModelKeyRequired | 2026-10-10 |
| presence | `internal/presence/presence.go` | a live grant is read as expired, so every gated act is refused | ed78c986dd6c | TestAgentStateFrames, TestBridgeToolsScopedToProject, TestBrowserSessionsListAndSignOut, TestBug296SandboxedSessionAdminOpsRefused, TestBug319DeleteRunningTerminalIDLeavesTerminalAlone (+99 more) | 2026-10-10 |
| projects | `internal/project/grant_widening.go` | adding an MCP to a project grant is not seen as a widening, so no approval is asked | ed78c986dd6c | TestProjectWidenApproved, TestProjectWidenDeniedCLI, TestProjectWidenDeniedHTTP | 2026-10-10 |
| remote | `internal/enrolment/budget.go` | the remote call budget admits one call over the cap | ed78c986dd6c | TestRemoteBudgetThrottles | 2026-10-10 |
| sandbox | `internal/sessions/sandbox/sandbox.go` | the loopback port deny is written for the wrong network direction | ed78c986dd6c | TestSandboxDeniedLoopbackPorts | 2026-10-10 |
| sealed | `cmd/relay/sealed_reset.go` | a sealed reset leaves settings.json in place | ed78c986dd6c | TestSealedReset | 2026-10-10 |
| services | `cmd/relay/service_routes.go` | an unknown service answers 400 instead of 404 | ed78c986dd6c | TestServiceListAndGet, TestServiceRegisterDeniedHTTP, TestServiceUnregister | 2026-10-10 |
| sessions | `cmd/relay/session_routes.go` | resuming an unknown session answers 400 instead of 404 | ed78c986dd6c | TestBug319ResumeHTTPDeletedDormantSessionIsNotFound, TestBug319ResumeStoppedDormantSessionIsNotFound, TestBug319ResumeStoppedSessionIsNotFound, TestSessionResume | 2026-10-10 |
| settings-ui | `cmd/relay/overview_seed.go` | the Overview config path shows the logs folder | ed78c986dd6c | TestStatusPaths | 2026-10-10 |
| templates | `cmd/relay/template_routes.go` | GET of an unknown template answers 400 instead of 404 | ed78c986dd6c | TestTemplateListAndGet | 2026-10-10 |

## Journey-only areas

None are marked.
