package main

// Regression coverage for two failure modes ADR-017's S6 brokering
// introduced into `relay mcp register` and `relay service register`:
//
//  1. the id was no longer settable and was always slugify(--name), which
//     silently diverges from a stored id a project's allowed_mcp_ids
//     already names once --name contains anything slugify collapses
//     differently than the id it was first registered under;
//  2. ServiceOps.Update applied every field in the request literally, so a
//     `register` that repeats only --command wiped --workdir, --url,
//     --autostart, --env and the --no-frontend-creds opt-out back to their
//     zero values.
//
// Both are exercised end to end through the real CLI parsing and the real
// bridge (newBrokerRouter + serveBroker), the same shape
// mcp_service_broker_test.go and service_cmd_test.go already use.

import "testing"

func TestMcpRegister_IDOverride(t *testing.T) {
	store := newCLISandboxStore(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

	// "fsMCP v3 (testfolder)" is the exact name from the regression report:
	// slugify would derive "fsmcp-v3-testfolder", not the "fsmcp3" a
	// project's allowed_mcp_ids might already name.
	mcpRegister([]string{
		"--name", "fsMCP v3 (testfolder)",
		"--id", "fsmcp3",
		"--command", buildTestMcpBinary(t),
	})

	mcps := store.Get().ExternalMcps
	if len(mcps) != 1 {
		t.Fatalf("want 1 registered mcp, got %d", len(mcps))
	}
	if mcps[0].ID != "fsmcp3" {
		t.Fatalf("id = %q, want the explicit --id %q", mcps[0].ID, "fsmcp3")
	}
}

func TestMcpRegister_IDOmittedDerivesFromName(t *testing.T) {
	store := newCLISandboxStore(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

	mcpRegister([]string{
		"--name", "Probe MCP",
		"--command", buildTestMcpBinary(t),
	})

	mcps := store.Get().ExternalMcps
	if len(mcps) != 1 {
		t.Fatalf("want 1 registered mcp, got %d", len(mcps))
	}
	if want := slugify("Probe MCP"); mcps[0].ID != want {
		t.Fatalf("id = %q, want slugify(name) = %q", mcps[0].ID, want)
	}
}

// TestMcpRegister_ReregisterWithSameIDUpdatesRecordInPlace is the exact
// operator story from the regression report: re-registering under the id a
// project grant already names — even though --name alone would slugify to
// something else — must land on the SAME record, not a second one.
func TestMcpRegister_ReregisterWithSameIDUpdatesRecordInPlace(t *testing.T) {
	store := newCLISandboxStore(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

	bin := buildTestMcpBinary(t)
	mcpRegister([]string{
		"--name", "fsMCP v3 (testfolder)",
		"--id", "fsmcp3",
		"--command", bin,
	})
	if got := len(store.Get().ExternalMcps); got != 1 {
		t.Fatalf("after first register: want 1 mcp, got %d", got)
	}

	mcpRegister([]string{
		"--name", "fsMCP v3 (testfolder)",
		"--id", "fsmcp3",
		"--command", bin,
		"--args", "changed",
	})

	mcps := store.Get().ExternalMcps
	if len(mcps) != 1 {
		t.Fatalf("re-registering under the same --id must update in place, not add a second record; got %d: %+v", len(mcps), mcps)
	}
	if mcps[0].ID != "fsmcp3" {
		t.Fatalf("id = %q, want fsmcp3", mcps[0].ID)
	}
	if len(mcps[0].Args) != 1 || mcps[0].Args[0] != "changed" {
		t.Fatalf("the second register's --args did not land: %+v", mcps[0].Args)
	}
}

func TestServiceRegister_IDOverride(t *testing.T) {
	store := newCLISandboxStore(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

	serviceRegister([]string{
		"--name", "fsMCP v3 (testfolder, read-only)",
		"--id", "fsmcp3ro",
		"--command", "/bin/true",
	})

	svcs := store.Get().Services
	if len(svcs) != 1 {
		t.Fatalf("want 1 registered service, got %d", len(svcs))
	}
	if svcs[0].ID != "fsmcp3ro" {
		t.Fatalf("id = %q, want the explicit --id %q", svcs[0].ID, "fsmcp3ro")
	}
}

func TestServiceRegister_IDOmittedDerivesFromName(t *testing.T) {
	store := newCLISandboxStore(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

	serviceRegister([]string{
		"--name", "Probe Svc",
		"--command", "/bin/true",
	})

	svcs := store.Get().Services
	if len(svcs) != 1 {
		t.Fatalf("want 1 registered service, got %d", len(svcs))
	}
	if want := slugify("Probe Svc"); svcs[0].ID != want {
		t.Fatalf("id = %q, want slugify(name) = %q", svcs[0].ID, want)
	}
}

// TestServiceRegister_ReregisterWithSameIDUpdatesRecordInPlace mirrors the
// mcp case above: a custom --id must dispatch to Update against the
// existing record rather than Create landing a second one under whatever
// slugify(--name) produces today.
func TestServiceRegister_ReregisterWithSameIDUpdatesRecordInPlace(t *testing.T) {
	store := newCLISandboxStore(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

	serviceRegister([]string{
		"--name", "fsMCP v3 (testfolder, read-only)",
		"--id", "fsmcp3ro",
		"--command", "/bin/old",
	})
	serviceRegister([]string{
		"--name", "fsMCP v3 (testfolder, read-only)",
		"--id", "fsmcp3ro",
		"--command", "/bin/new",
	})

	svcs := store.Get().Services
	if len(svcs) != 1 {
		t.Fatalf("re-registering under the same --id must update in place, not add a second record; got %d: %+v", len(svcs), svcs)
	}
	if svcs[0].Command != "/bin/new" {
		t.Fatalf("command = %q, want the second register's command to have landed", svcs[0].Command)
	}
}

// TestServiceRegister_RepeatingOnlyCommandPreservesEverythingElse is
// regression 2 from the report: ServiceOps.Update used to apply every field
// in the request literally, so a `register` that repeated only --command
// wiped --workdir, --url, --autostart, --env and the --no-frontend-creds
// opt-out back to their zero values. A flag left off the second call must
// leave the stored value alone.
func TestServiceRegister_RepeatingOnlyCommandPreservesEverythingElse(t *testing.T) {
	store := newCLISandboxStore(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

	serviceRegister([]string{
		"--name", "Backend Svc",
		"--command", "/bin/old",
		"--workdir", "/tmp",
		"--url", "http://127.0.0.1:9000",
		"--autostart",
		"--env", "FOO=bar",
		"--no-frontend-creds",
	})

	serviceRegister([]string{
		"--name", "Backend Svc",
		"--command", "/bin/new",
	})

	svcs := store.Get().Services
	if len(svcs) != 1 {
		t.Fatalf("want exactly 1 service, got %d", len(svcs))
	}
	cfg := svcs[0]
	if cfg.Command != "/bin/new" {
		t.Fatalf("command = %q, want the second register's command to have landed", cfg.Command)
	}
	if cfg.WorkingDir == "" {
		t.Error("workdir was cleared by a register that did not repeat --workdir")
	}
	if cfg.URL != "http://127.0.0.1:9000" {
		t.Errorf("url = %q, want it preserved from the first register", cfg.URL)
	}
	if !cfg.Autostart {
		t.Error("autostart was cleared by a register that did not repeat --autostart")
	}
	if got, _ := cfg.Env["FOO"].Reveal(); got != "bar" {
		t.Errorf("env FOO = %q, want it preserved from the first register", got)
	}
	if cfg.FrontendConsumer == nil || *cfg.FrontendConsumer {
		t.Errorf("FrontendConsumer = %v, want the --no-frontend-creds opt-out preserved", cfg.FrontendConsumer)
	}
}

// TestServiceRegister_ExplicitAutostartFalseTurnsItOff is the sharper half
// of the same fix: absent must preserve, but an explicit --autostart=false
// must still apply — the CLI flag's zero value is a real value, not a
// synonym for "not given".
func TestServiceRegister_ExplicitAutostartFalseTurnsItOff(t *testing.T) {
	store := newCLISandboxStore(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

	serviceRegister([]string{
		"--name", "Backend Svc",
		"--command", "/bin/old",
		"--autostart",
	})
	serviceRegister([]string{
		"--name", "Backend Svc",
		"--command", "/bin/old",
		"--autostart=false",
	})

	svcs := store.Get().Services
	if len(svcs) != 1 {
		t.Fatalf("want exactly 1 service, got %d", len(svcs))
	}
	if svcs[0].Autostart {
		t.Error("--autostart=false did not turn autostart off")
	}
}
