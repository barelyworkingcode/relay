package main

// The tray is the only reader of configuration for a normal CLI command.
// Three properties are pinned here for every read command: its stdout over
// the bridge equals what the retired direct-disk read printed for the same
// seeded settings (the legacy* renderers below are that retired code), a
// stopped tray refuses by name without reading the file, and no response
// carries secret material.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/control"
	"github.com/barelyworkingcode/relay/internal/presence"
	"github.com/barelyworkingcode/relay/internal/service"
)

const (
	leakHash   = "LEAKHASH0123456789abcdef"
	leakSecret = "LEAKSECRETVALUE"
	leakX      = "LEAKXCOORD"
	leakY      = "LEAKYCOORD"
)

var leakMarkers = []string{leakHash, leakSecret, leakX, leakY, "LEAKOAUTH", "TOKENPLAINTEXT"}

// forbiddenResponseKeys are JSON key fragments no read response may carry:
// each names a class of value that must never cross the bridge.
var (
	forbiddenResponseKeyFragments = []string{"hash", "token", "secret", "env", "oauth", "private", "password", "sealed"}
	forbiddenResponseKeys         = []string{"key", "x", "y", "public_key"}
)

func seedReadFixtures(t *testing.T, store config.SettingsStore) {
	t.Helper()
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.APICredentials = []config.APICredential{
			{ID: "cred-forever", Name: "forever", Hash: leakHash + "1", Classes: []control.CapabilityClass{control.ClassRead}, Created: "2026-09-01T00:00:00Z"},
			{ID: "cred-future", Name: "future", Hash: leakHash + "2", Classes: []control.CapabilityClass{control.ClassRead, control.ClassProxy}, Created: "2026-09-02T00:00:00Z", Expires: future},
			{ID: "cred-dead", Name: "dead", Hash: leakHash + "3", Classes: []control.CapabilityClass{control.ClassGrant}, Created: "2026-09-03T00:00:00Z", Expires: past},
		}
		s.Services = []config.ServiceConfig{
			{ID: "sched", DisplayName: "Scheduler", Command: "/usr/bin/sched", Args: []string{"--fast", "x"}, URL: "http://127.0.0.1:9", Autostart: true,
				Env: map[string]config.Secret{"API_KEY": config.NewSecret(leakSecret)}, WorkingDir: "/tmp/w",
				Capabilities: []config.ServiceCapability{config.ServiceCapabilityManifest}},
			{ID: "plain", DisplayName: "Plain", Command: "/bin/true", Capabilities: []config.ServiceCapability{}},
		}
		s.ExternalMcps = []config.ExternalMcp{
			{ID: "fs", DisplayName: "Files", Command: "/usr/bin/fsmcp", Args: []string{"--root", "/tmp"}, Env: map[string]config.Secret{"K": config.NewSecret(leakSecret)}},
			{ID: "web", DisplayName: "Web", Transport: "http", URL: "https://mcp.example.test/mcp",
				OAuthState: &config.OAuthState{AccessToken: config.NewSecret("LEAKOAUTH")}},
		}
		s.Passkeys = []config.Passkey{
			{ID: "cred-id-0123456789abcdef", Name: "laptop", X: []byte(leakX), Y: []byte(leakY), Created: "2026-08-28T00:00:00Z", SignCount: 7},
		}
		s.EvePasskeys = []config.EvePasskey{
			{ID: "eve-id-0123456789abcdef", Label: "iPhone", Created: "2026-09-07T10:00:00Z", LastUsed: "2026-09-07T11:00:00Z"},
			{ID: "eve-id-fedcba9876543210", Label: "MacBook", Created: "2026-09-06T10:00:00Z"},
		}
		s.EvePasskeyRevocations = []config.EvePasskeyRevocation{{ID: "eve-id-fedcba9876543210", Requested: "2026-09-07T12:00:00Z"}}
		s.Projects = []config.Project{
			{ID: "local1", Name: "Zed Local", Path: "/tmp/zed", AllowedMcpIDs: []string{"*"}, Token: config.NewSecret("TOKENPLAINTEXT"), TokenHash: leakHash + "p",
				DisabledTools: map[string][]string{"fs": {"a", "b"}},
				Mounts:        []config.MountGrant{{ID: "m1", Path: "/tmp/m", Access: "read"}}},
			{ID: "remote1", Name: "Alpha Profile", Kind: config.ProjectKindRemote, AllowedMcpIDs: []string{"fs"}, Token: config.NewSecret("TOKENPLAINTEXT"), TokenHash: leakHash + "r",
				AllowedTools: map[string][]string{"fs": {"read_*"}},
				Context:      map[string]json.RawMessage{"fs": json.RawMessage(`{"allowed-dir":"/"}`)}},
		}
		s.Enrolments = []config.Enrolment{
			{ClientID: "hermes", Fingerprint: "sha256:" + strings.Repeat("ab", 32), ProjectIDs: []string{"remote1"}, CLIAdmin: true,
				Budget: config.EnrolmentBudget{WindowSeconds: 60, MaxCalls: 5, MaxResultBytes: 1000, MountMaxOps: 3}, CreatedAt: "2026-09-01T00:00:00Z"},
			{ClientID: "quiet", Fingerprint: "sha256:" + strings.Repeat("cd", 32), Budget: config.EnrolmentBudget{WindowSeconds: 30, MaxCalls: 2}, CreatedAt: "2026-09-02T00:00:00Z"},
		}
	}), "seed read fixtures")
}

func legacyTab(b *bytes.Buffer) *tabwriter.Writer { return tabwriter.NewWriter(b, 0, 4, 2, ' ', 0) }

func legacyCredentialList(s *config.Settings, includeExpired bool, now time.Time) string {
	var b bytes.Buffer
	shown := []config.APICredential{}
	for _, c := range s.APICredentials {
		if includeExpired || !c.Expired(now) {
			shown = append(shown, c)
		}
	}
	if len(shown) == 0 {
		return "no credentials\n"
	}
	w := legacyTab(&b)
	fmt.Fprintln(w, "ID\tNAME\tCLASSES\tCREATED\tEXPIRES")
	for _, c := range shown {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", c.ID, c.Name, formatClasses(c.Classes), c.Created, formatCredentialExpiry(c, now))
	}
	w.Flush()
	return b.String()
}

func legacyServiceList(s *config.Settings, statuses map[string]service.SupervisionStatus) string {
	var b bytes.Buffer
	w := legacyTab(&b)
	fmt.Fprintln(w, "ID\tNAME\tCOMMAND\tURL\tAUTOSTART\tCAPABILITIES\tSTATE")
	for _, svc := range s.Services {
		cmd := svc.Command
		if len(svc.Args) > 0 {
			cmd += " " + strings.Join(svc.Args, " ")
		}
		auto := "no"
		if svc.Autostart {
			auto = "yes"
		}
		urlStr := svc.URL
		if urlStr == "" {
			urlStr = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", svc.ID, svc.DisplayName, cmd, urlStr, auto, capabilitiesColumn(svc.Capabilities), serviceStateColumn(svc.ID, statuses))
	}
	w.Flush()
	return b.String()
}

func legacyMcpList(s *config.Settings) string {
	var b bytes.Buffer
	w := legacyTab(&b)
	fmt.Fprintln(w, "ID\tNAME\tTRANSPORT\tENDPOINT")
	for _, m := range s.ExternalMcps {
		transport := m.Transport
		if transport == "" {
			transport = "stdio"
		}
		var endpoint string
		if m.IsHTTP() {
			endpoint = m.URL
		} else {
			endpoint = m.Command
			if len(m.Args) > 0 {
				endpoint += " " + strings.Join(m.Args, " ")
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", m.ID, m.DisplayName, transport, endpoint)
	}
	w.Flush()
	return b.String()
}

func legacyLoginList(s *config.Settings) string {
	var b bytes.Buffer
	w := legacyTab(&b)
	fmt.Fprintln(w, "NAME\tCREDENTIAL ID\tCREATED\tSIGN COUNT")
	for _, p := range s.Passkeys {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", p.Name, abbreviatePasskeyID(p.ID), p.Created, p.SignCount)
	}
	w.Flush()
	return b.String()
}

func legacyEveList(s *config.Settings) string {
	var b bytes.Buffer
	w := legacyTab(&b)
	fmt.Fprintln(w, "LABEL\tCREDENTIAL ID\tCREATED\tLAST USED\tSTATUS")
	for _, p := range evePasskeyViews(s) {
		status := "-"
		if p.RevocationPending {
			status = "revocation pending"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Label, p.Short, p.Created, p.LastUsed, status)
	}
	w.Flush()
	return b.String()
}

func legacyEnrolList(s *config.Settings) string {
	var b bytes.Buffer
	w := legacyTab(&b)
	fmt.Fprintln(w, "CLIENT ID\tPROFILES\tCLI-ADMIN\tCALLS/WINDOW\tBYTES/WINDOW\tMOUNT-OPS/WINDOW\tMOUNT-READ/WINDOW\tMOUNT-WRITE/WINDOW\tCREATED\tFINGERPRINT")
	for _, e := range s.Enrolments {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d/%ds\t%d\t%d\t%d\t%d\t%s\t%s\n",
			e.ClientID, formatGrants(e.ProjectIDs), formatCLIAdmin(e.CLIAdmin),
			e.Budget.MaxCalls, e.Budget.WindowSeconds, e.Budget.MaxResultBytes,
			e.Budget.MountMaxOps, e.Budget.MountMaxReadBytes, e.Budget.MountMaxWriteBytes,
			e.CreatedAt, e.Fingerprint)
	}
	w.Flush()
	return b.String()
}

func legacyGrant(s *config.Settings, selector string, asJSON bool) string {
	var b bytes.Buffer
	records := selectGrantRecords(s.Projects, selector)
	views := make([]grantView, 0, len(records))
	for _, p := range records {
		views = append(views, newGrantView(s, p))
	}
	if asJSON {
		enc := json.NewEncoder(&b)
		enc.SetIndent("", "  ")
		_ = enc.Encode(views)
		return b.String()
	}
	printGrantViews(&b, views)
	return b.String()
}

// supervisedManager reports fixed supervision state, the one thing the tray
// holds in memory that `service list` shows.
type supervisedManager struct {
	noopServiceManager
	statuses map[string]service.SupervisionStatus
}

func (m supervisedManager) SupervisionStatuses() map[string]service.SupervisionStatus {
	return m.statuses
}

func seededTray(t *testing.T) config.SettingsStore {
	t.Helper()
	store := newCLISandboxStore(t)
	seedReadFixtures(t, store)
	serveBroker(t, newBrokerRouter(t, store, func(r *appRouter) {
		r.serviceOps.Registry = supervisedManager{statuses: map[string]service.SupervisionStatus{
			"sched": {Phase: service.SupervisionFailed, LastExitCode: 3},
			"plain": {Phase: service.SupervisionRunning},
		}}
	}))
	return store
}

func requireSame(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: tray-read output differs from the retired direct read\n--- got\n%s\n--- want\n%s", what, got, want)
	}
}

func TestReadCommands_OutputEqualsTheRetiredDirectRead(t *testing.T) {
	store := seededTray(t)
	s := store.Get()
	statuses := map[string]service.SupervisionStatus{
		"sched": {Phase: service.SupervisionFailed, LastExitCode: 3},
		"plain": {Phase: service.SupervisionRunning},
	}

	t.Run("credential list", func(t *testing.T) {
		requireSame(t, "default", captureStdout(t, func() { credentialList(nil) }), legacyCredentialList(s, false, time.Now()))
		requireSame(t, "include-expired", captureStdout(t, func() { credentialList([]string{"--include-expired"}) }), legacyCredentialList(s, true, time.Now()))
	})
	t.Run("service list", func(t *testing.T) {
		got := captureStdout(t, func() { serviceList() })
		requireSame(t, "service list", got, legacyServiceList(s, statuses))
		if !strings.Contains(got, "failed (exit 3)") || !strings.Contains(got, "running") {
			t.Fatalf("STATE column did not carry the tray's supervision state:\n%s", got)
		}
	})
	t.Run("mcp list", func(t *testing.T) {
		requireSame(t, "mcp list", captureStdout(t, func() { mcpList() }), legacyMcpList(s))
	})
	t.Run("login list", func(t *testing.T) {
		requireSame(t, "login list", captureStdout(t, func() { loginList() }), legacyLoginList(s))
	})
	t.Run("eve list", func(t *testing.T) {
		requireSame(t, "eve list", captureStdout(t, func() { eveList() }), legacyEveList(s))
	})
	t.Run("enrol list", func(t *testing.T) {
		requireSame(t, "enrol list", captureStdout(t, func() { enrolList() }), legacyEnrolList(s))
	})
	t.Run("grant", func(t *testing.T) {
		requireSame(t, "grant", captureStdout(t, func() { runGrantCommand(nil) }), legacyGrant(s, "", false))
		requireSame(t, "grant --json", captureStdout(t, func() { runGrantCommand([]string{"--json"}) }), legacyGrant(s, "", true))
		requireSame(t, "grant --project by name", captureStdout(t, func() { runGrantCommand([]string{"--project", "Alpha Profile"}) }), legacyGrant(s, "Alpha Profile", false))
		requireSame(t, "grant --project by id", captureStdout(t, func() { runGrantCommand([]string{"--project", "local1"}) }), legacyGrant(s, "local1", false))
	})
}

func TestReadCommands_EmptyConfigurationMessagesAreUnchanged(t *testing.T) {
	store := newCLISandboxStore(t)
	serveBroker(t, newBrokerRouter(t, store, nil))
	for _, c := range []struct {
		name string
		run  func()
		want string
	}{
		{"credential", func() { credentialList(nil) }, "no credentials\n"},
		{"service", func() { serviceList() }, "no services registered\n"},
		{"mcp", func() { mcpList() }, "no mcp servers registered\n"},
		{"login", func() { loginList() }, "no passkeys registered\n"},
		{"eve", func() { eveList() }, "no eve passkeys reported\n"},
		{"enrol", func() { enrolList() }, "no enrolments\n"},
		{"grant", func() { runGrantCommand(nil) }, "no projects or access profiles\n"},
	} {
		requireSame(t, c.name, captureStdout(t, c.run), c.want)
	}
}

// The tray reads a fresh snapshot: a change made to settings.json by another
// process (the way an operator's edit lands) is what the next list shows.
func TestReadCommands_ShowWhatTheTrayHoldsNow(t *testing.T) {
	store := newCLISandboxStore(t)
	serveBroker(t, newBrokerRouter(t, store, nil))

	assertNoErr(t, store.With(func(s *config.Settings) {
		s.ExternalMcps = []config.ExternalMcp{{ID: "later", DisplayName: "Later", Command: "/bin/true"}}
	}), "seed after the tray started")

	if out := captureStdout(t, func() { mcpList() }); !strings.Contains(out, "later") {
		t.Fatalf("mcp list did not show the record written after the tray started:\n%s", out)
	}
}

func TestReadOps_ResponsesCarryNoSecretMaterial(t *testing.T) {
	store := newCLISandboxStore(t)
	seedReadFixtures(t, store)
	r := newBrokerRouter(t, store, nil)

	readOps := []struct {
		op   string
		args string
	}{
		{"credential.list", ``},
		{"service.list", ``},
		{"mcp.list", ``},
		{"login.list", ``},
		{"eve.list", ``},
		{"enrolment.list", ``},
		{"grant.view", ``},
		{"grant.view", `{"project":"remote1"}`},
	}
	for _, c := range readOps {
		t.Run(c.op+c.args, func(t *testing.T) {
			raw, err := r.AdminOp(context.Background(), c.op, json.RawMessage(c.args))
			assertNoErr(t, err, c.op)
			if len(raw) < 3 {
				t.Fatalf("%s returned an empty answer %q; the leak scan would pass vacuously", c.op, raw)
			}
			for _, marker := range leakMarkers {
				if bytes.Contains(raw, []byte(marker)) {
					t.Fatalf("%s response carries secret material %q:\n%s", c.op, marker, raw)
				}
			}
			var generic any
			assertNoErr(t, json.Unmarshal(raw, &generic), "decode "+c.op)
			walkKeys(generic, func(key string) {
				lower := strings.ToLower(key)
				for _, frag := range forbiddenResponseKeyFragments {
					if strings.Contains(lower, frag) {
						t.Errorf("%s response has key %q, which names secret material (%q):\n%s", c.op, key, frag, raw)
					}
				}
				if slices.Contains(forbiddenResponseKeys, lower) {
					t.Errorf("%s response has key %q, which names key material:\n%s", c.op, key, raw)
				}
			})
		})
	}
}

func walkKeys(v any, visit func(string)) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			visit(k)
			walkKeys(child, visit)
		}
	case []any:
		for _, child := range x {
			walkKeys(child, visit)
		}
	}
}

// The leak scan has teeth: the seeded record really holds the secret
// material, so a response that echoed it would trip the scan.
func TestReadOps_LeakScanFixturesHoldTheSecrets(t *testing.T) {
	store := newCLISandboxStore(t)
	seedReadFixtures(t, store)
	s := store.Get()
	if len(s.Passkeys) == 0 || string(s.Passkeys[0].X) != leakX || string(s.Passkeys[0].Y) != leakY {
		t.Fatal("fixture passkey lost its coordinates")
	}
	if len(s.APICredentials) == 0 || !strings.HasPrefix(s.APICredentials[0].Hash, leakHash) {
		t.Fatal("fixture credential lost its hash")
	}
	if len(s.Services) == 0 {
		t.Fatal("fixture has no service")
	}
	if v, ok := s.Services[0].Env["API_KEY"].Reveal(); !ok || v != leakSecret {
		t.Fatal("fixture service lost its env secret")
	}
}

func TestReadOps_AreNotPresenceGatedOrRemote(t *testing.T) {
	for _, op := range []string{"credential.list", "service.list", "mcp.list", "login.list", "eve.list", "enrolment.list", "grant.view"} {
		if _, ok := adminOps[op]; !ok {
			t.Errorf("adminOps has no %q", op)
		}
		if slices.Contains(presence.GatedOps, op) {
			t.Errorf("read op %q is in presence.GatedOps: a read must never prompt", op)
		}
	}
}

func TestReadOps_NameResolutionHappensTrayside(t *testing.T) {
	store := newCLISandboxStore(t)
	seedReadFixtures(t, store)
	r := newBrokerRouter(t, store, nil)
	ctx := context.Background()

	_, err := r.AdminOp(ctx, "service.restart", json.RawMessage(`{"name":"No Such"}`))
	if err == nil || !strings.Contains(err.Error(), `no service found with name "No Such"`) {
		t.Fatalf("restart by unknown name: %v", err)
	}
	_, err = r.AdminOp(ctx, "service.unregister", json.RawMessage(`{"id":"nope"}`))
	if err == nil || !strings.Contains(err.Error(), `no service found with id "nope"`) {
		t.Fatalf("unregister by unknown id: %v", err)
	}
	_, err = r.AdminOp(ctx, "mcp.unregister", json.RawMessage(`{"name":"Nope"}`))
	if err == nil || !strings.Contains(err.Error(), `no mcp found with name "Nope"`) {
		t.Fatalf("mcp unregister by unknown name: %v", err)
	}

	raw, err := r.AdminOp(ctx, "service.unregister", json.RawMessage(`{"name":"Plain"}`))
	assertNoErr(t, err, "unregister by name")
	if !strings.Contains(string(raw), `"plain"`) {
		t.Fatalf("unregister by name did not answer with the resolved id: %s", raw)
	}
	raw, err = r.AdminOp(ctx, "mcp.unregister", json.RawMessage(`{"name":"Web"}`))
	assertNoErr(t, err, "mcp unregister by name")
	if !strings.Contains(string(raw), `"web"`) {
		t.Fatalf("mcp unregister by name did not answer with the resolved id: %s", raw)
	}
	for _, m := range store.Get().ExternalMcps {
		if m.ID == "web" {
			t.Fatal("mcp web survived unregister by name")
		}
	}
}

func TestServiceUnregisterAndRestart_CLIResolveNamesThroughTheTray(t *testing.T) {
	store := newCLISandboxStore(t)
	seedReadFixtures(t, store)
	serveBroker(t, newBrokerRouter(t, store, nil))

	if out := captureStdout(t, func() { serviceRestart([]string{"--name", "Scheduler"}) }); out != "restarting service \"sched\"\n" {
		t.Fatalf("restart by name printed %q", out)
	}
	if out := captureStdout(t, func() { serviceUnregister([]string{"--name", "Scheduler"}) }); out != "unregistered service \"sched\"\n" {
		t.Fatalf("unregister by name printed %q", out)
	}
	if out := captureStdout(t, func() { mcpUnregister([]string{"--name", "Files"}) }); out != "unregistered mcp \"fs\"\n" {
		t.Fatalf("mcp unregister by name printed %q", out)
	}
}

// With no tray, each read command refuses by name and never opens the
// settings file: a planted file with a distinctive record stays byte-for-byte
// and mod-time untouched, and its contents never reach the output.
func TestReadCommands_StoppedTrayRefusesWithoutReadingSettings(t *testing.T) {
	commands := []struct {
		name string
		args []string
	}{
		{"relay credential list", []string{"credential", "list"}},
		{"relay service list", []string{"service", "list"}},
		{"relay mcp list", []string{"mcp", "list"}},
		{"relay login list", []string{"login", "list"}},
		{"relay eve list", []string{"eve", "list"}},
		{"relay enrol list", []string{"enrol", "list"}},
		{"relay grant", []string{"grant"}},
		{"relay service unregister", []string{"service", "unregister", "--name", "PlantedName"}},
		{"relay service restart", []string{"service", "restart", "--name", "PlantedName"}},
		{"relay mcp unregister", []string{"mcp", "unregister", "--name", "PlantedName"}},
	}
	for _, c := range commands {
		t.Run(c.name, func(t *testing.T) {
			dir := mkShortTempDir(t, "relay-stopped-")
			planted := []byte(`{"version":1,"projects":[{"id":"planted","name":"PlantedName"}],"external_mcps":[{"id":"planted","display_name":"PlantedName"}],"services":[{"id":"planted","display_name":"PlantedName"}]}`)
			path := filepath.Join(dir, "settings.json")
			assertNoErr(t, os.WriteFile(path, planted, 0600), "plant settings.json")
			old := time.Now().Add(-48 * time.Hour)
			assertNoErr(t, os.Chtimes(path, old, old), "age settings.json")

			out, code := runCLISubprocess(t, dir, c.args...)

			if code == 0 {
				t.Fatalf("exited 0 with no service running:\n%s", out)
			}
			if !strings.Contains(out, c.name) || !strings.Contains(out, "requires the service") {
				t.Fatalf("refusal does not name %q and say it requires the service: %q", c.name, out)
			}
			if strings.Contains(out, "PlantedName") || strings.Contains(out, "planted") {
				t.Fatalf("output shows content of the settings file, so it was read: %q", out)
			}
			after, err := os.ReadFile(path)
			assertNoErr(t, err, "re-read settings.json")
			if !bytes.Equal(after, planted) {
				t.Fatalf("settings.json was rewritten: %s", after)
			}
			st, err := os.Stat(path)
			assertNoErr(t, err, "stat settings.json")
			if st.ModTime().After(old.Add(time.Minute)) {
				t.Fatalf("settings.json mod-time moved to %v", st.ModTime())
			}
		})
	}
}

func TestServiceRequiredMessage_DoesNotClaimReadsWorkWithRelayStopped(t *testing.T) {
	msg := serviceRequiredMessage("relay credential list") + sshRefusalMessage
	for _, stale := range []string{"Read commands still work", "every `list`.", "unaffected"} {
		if strings.Contains(msg, stale) {
			t.Errorf("refusal text still says %q", stale)
		}
	}
	if !strings.Contains(msg, "relay audit") {
		t.Error("refusal text no longer names the commands that do work stopped")
	}
}

// readCommandFuncs are the CLI functions behind the tray-only reads. None
// may construct a settings store: the running tray is their only reader.
var readCommandFuncs = []string{
	"runCredentialCommand", "credentialList",
	"runServiceCommand", "serviceList", "serviceUnregister", "serviceRestart",
	"runMcpCommand", "mcpList", "mcpUnregister",
	"runLoginCommand", "loginList",
	"runEveCommand", "eveList",
	"runEnrolCommand", "enrolList",
	"runGrantCommand",
}

func TestReadCommands_NeverConstructASettingsStore(t *testing.T) {
	fset := token.NewFileSet()
	root := relaySourceDir(t)
	entries, err := os.ReadDir(root)
	assertNoErr(t, err, "read source dir")

	found := map[string]bool{}
	var offenders []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(root, name), nil, 0)
		assertNoErr(t, err, "parse "+name)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil || !slices.Contains(readCommandFuncs, fd.Name.Name) {
				continue
			}
			found[fd.Name.Name] = true
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if ok && strings.HasPrefix(sel.Sel.Name, "NewSettingsStore") {
					offenders = append(offenders, fd.Name.Name+" calls "+sel.Sel.Name)
				}
				return true
			})
		}
	}
	var missing []string
	for _, fn := range readCommandFuncs {
		if !found[fn] {
			missing = append(missing, fn)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("guard scanned nothing for %v — renamed or moved? update readCommandFuncs", missing)
	}
	if len(offenders) > 0 {
		t.Fatalf("read commands must go through the tray, not a settings store: %v", offenders)
	}
}
