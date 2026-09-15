package main

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/jsonrpc"
)

var (
	capsFrontend = []config.ServiceCapability{config.ServiceCapabilityFrontend}
	capsBridge   = []config.ServiceCapability{config.ServiceCapabilityManifest}
)

// bridgeOpCapability restates docs/launch-identity.md's table for the bridge
// operations, independently of service.Allowed.
var bridgeOpCapability = map[string]config.ServiceCapability{
	bridge.ReqRegisterManifest:  config.ServiceCapabilityManifest,
	bridge.ReqRegisterModelHost: config.ServiceCapabilityModelHost,
}

// Every capability set is exercised through relay's real bridge socket and
// real frontend socket, bound to this process's own audit token: Hello always
// succeeds, and each operation is reachable exactly when its capability is
// held.
func TestLaunchCapabilities_EachSetReachesExactlyItsOperations(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps []config.ServiceCapability
	}{
		{"frontend", capsFrontend},
		{"manifest", []config.ServiceCapability{config.ServiceCapabilityManifest}},
		{"model_host", []config.ServiceCapability{config.ServiceCapabilityModelHost}},
		{"frontend+manifest (scheduler)", []config.ServiceCapability{config.ServiceCapabilityFrontend, config.ServiceCapabilityManifest}},
		{"none", []config.ServiceCapability{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := startIdentityBridge(t)
			const launch = "svc-caps"
			secret, _ := b.begin(t, launch, tc.caps)
			if resp, line := b.hello(t, launch, secret); resp.Type != bridge.RespOK {
				t.Fatalf("Hello with %v: %s", tc.caps, line)
			}

			for op, req := range serviceOperationRequests(t, launch) {
				resp, line := b.send(t, req)
				refused := resp.Type == bridge.RespError && resp.Code == jsonrpc.CodeUnauthorized
				if want := slices.Contains(tc.caps, bridgeOpCapability[op]); refused == want {
					t.Errorf("%s: refused = %v, want allowed = %v (%s)", op, refused, want, line)
				}
			}

			// plan-broker-and-sessions.md §2 C1 deletes OpServiceTools: no
			// capability set ever grants a service's launch identity a
			// tokenless ListTools/CallTool path anymore.
			resp, line := b.send(t, bridge.BridgeRequest{Type: bridge.ReqListTools})
			if resp.Type == bridge.RespTools {
				t.Errorf("tokenless ListTools listed tools for capabilities %v: %s", tc.caps, line)
			}
			if !strings.Contains(resp.Message, "token") {
				t.Errorf("tokenless ListTools must fall to directory auth regardless of capabilities, got %q", resp.Message)
			}
			if manifestHeld := slices.Contains(tc.caps, config.ServiceCapabilityManifest); (b.enhanced.Get(launch) != nil) != manifestHeld {
				t.Errorf("manifest registered = %v, want %v", b.enhanced.Get(launch) != nil, manifestHeld)
			}

			srv, launches, _ := startIdentityFrontend(t, NewEnhancedServiceRegistry(nil))
			bindSelf(t, launches, launch, tc.caps)
			status := frontendStatus(t, dialFrontendHTTP(srv.socketPath), "GET", "http://unix/api/services", "")
			wantFrontend := slices.Contains(tc.caps, config.ServiceCapabilityFrontend)
			if (status == http.StatusOK) != wantFrontend || (!wantFrontend && status != http.StatusUnauthorized) {
				t.Errorf("frontend socket: status = %d, want frontend access = %v", status, wantFrontend)
			}
		})
	}
}

func TestServiceOps_CreateRefusesAnUnknownCapabilityBeforePersisting(t *testing.T) {
	store := newCLISandboxStore(t)
	ops := &ServiceOps{Store: store, Registry: &svcRecorder{}, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
	caps := []config.ServiceCapability{config.ServiceCapabilityManifest, "admin"}
	if _, err := ops.Create(context.Background(), serviceFields{DisplayName: "Bad Caps", Command: "/bin/true", Capabilities: &caps}, auditViaCLI, ""); err == nil {
		t.Fatal("a service with an unknown capability was created")
	}
	if len(store.Get().Services) != 0 {
		t.Fatalf("a refused create persisted a record: %+v", store.Get().Services)
	}
}

func TestServiceOps_UpdateWithoutCapabilitiesKeepsTheStoredSet(t *testing.T) {
	store := newCLISandboxStore(t)
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.UpsertService(config.ServiceConfig{ID: "backend", DisplayName: "Backend", Command: "/bin/old", Capabilities: []config.ServiceCapability{config.ServiceCapabilityManifest}})
	}), "seed")
	ops := &ServiceOps{Store: store, Registry: &svcRecorder{}, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}

	_, err := ops.Update(context.Background(), "backend", serviceFields{DisplayName: "Backend", Command: "/bin/new"}, auditViaIPC, "")
	assertNoErr(t, err, "Update")

	svc, _ := config.FindServiceByID(store.Get(), "backend")
	if svc == nil || !slices.Equal(svc.Capabilities, []config.ServiceCapability{config.ServiceCapabilityManifest}) {
		t.Fatalf("an edit that never mentioned capabilities changed them: %+v", svc)
	}
	if svc.Command != "/bin/new" {
		t.Fatalf("the edit itself must still apply, got %q", svc.Command)
	}
}
