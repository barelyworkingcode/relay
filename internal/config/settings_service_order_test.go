package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func svcOrderFixture() *Settings {
	return &Settings{Services: []ServiceConfig{
		{ID: "a", DisplayName: "A"},
		{ID: "b", DisplayName: "B"},
		{ID: "c", DisplayName: "C"},
		{ID: "d", DisplayName: "D"},
	}}
}

func svcOrderIDs(s *Settings) []string {
	ids := make([]string, 0, len(s.Services))
	for _, svc := range s.Services {
		ids = append(ids, svc.ID)
	}
	return ids
}

func TestMoveService_Reorders(t *testing.T) {
	cases := []struct {
		name  string
		id    string
		index int
		want  []string
	}{
		{"first to last", "a", 3, []string{"b", "c", "d", "a"}},
		{"last to first", "d", 0, []string{"d", "a", "b", "c"}},
		{"middle up", "c", 1, []string{"a", "c", "b", "d"}},
		{"middle down", "b", 2, []string{"a", "c", "b", "d"}},
		{"middle to first", "c", 0, []string{"c", "a", "b", "d"}},
		{"middle to last", "b", 3, []string{"a", "c", "d", "b"}},
		{"same index is a no-op", "b", 1, []string{"a", "b", "c", "d"}},
		{"first stays first", "a", 0, []string{"a", "b", "c", "d"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := svcOrderFixture()
			if err := s.MoveService(tc.id, tc.index); err != nil {
				t.Fatalf("MoveService(%q, %d) = %v, want nil", tc.id, tc.index, err)
			}
			if got := svcOrderIDs(s); !slices.Equal(got, tc.want) {
				t.Fatalf("order = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMoveService_KeepsTheRecordIntact(t *testing.T) {
	s := &Settings{Services: []ServiceConfig{
		{ID: "a", DisplayName: "A", Command: "/bin/a", Autostart: true, HideFromMenu: true},
		{ID: "b", DisplayName: "B", Command: "/bin/b"},
	}}
	if err := s.MoveService("a", 1); err != nil {
		t.Fatalf("MoveService: %v", err)
	}
	got := s.Services[1]
	if got.ID != "a" || got.Command != "/bin/a" || !got.Autostart || !got.HideFromMenu {
		t.Fatalf("moved record changed: %+v", got)
	}
}

func TestMoveService_Refusals(t *testing.T) {
	cases := []struct {
		name    string
		id      string
		index   int
		wantErr error
	}{
		{"unknown id", "nope", 0, ErrServiceNotFound},
		{"negative index", "b", -1, ErrIndexOutOfRange},
		{"index equal to length", "b", 4, ErrIndexOutOfRange},
		{"index far past end", "b", 99, ErrIndexOutOfRange},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := svcOrderFixture()
			before := svcOrderIDs(s)
			err := s.MoveService(tc.id, tc.index)
			if err == nil {
				t.Fatalf("MoveService(%q, %d) = nil, want error", tc.id, tc.index)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("MoveService(%q, %d) = %v, want errors.Is %v", tc.id, tc.index, err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.id) {
				t.Fatalf("error %q does not name the id %q", err, tc.id)
			}
			if got := svcOrderIDs(s); !slices.Equal(got, before) {
				t.Fatalf("refused move changed the order: %v, want %v", got, before)
			}
		})
	}
}

func TestMoveService_EmptyList(t *testing.T) {
	s := &Settings{}
	if err := s.MoveService("a", 0); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("MoveService on empty list = %v, want ErrServiceNotFound", err)
	}
}

func TestSetServiceMenuHidden(t *testing.T) {
	s := svcOrderFixture()

	s.SetServiceMenuHidden("b", true)
	for _, svc := range s.Services {
		if svc.HideFromMenu != (svc.ID == "b") {
			t.Fatalf("after hiding b: %s HideFromMenu = %v", svc.ID, svc.HideFromMenu)
		}
	}

	s.SetServiceMenuHidden("b", false)
	for _, svc := range s.Services {
		if svc.HideFromMenu {
			t.Fatalf("after showing b: %s still hidden", svc.ID)
		}
	}

	before := svcOrderIDs(s)
	s.SetServiceMenuHidden("unknown", true)
	if got := svcOrderIDs(s); !slices.Equal(got, before) {
		t.Fatalf("unknown id changed the list: %v", got)
	}
	for _, svc := range s.Services {
		if svc.HideFromMenu {
			t.Fatalf("unknown id hid %s", svc.ID)
		}
	}
}

func TestSetServiceMenuHidden_SessionHostRow(t *testing.T) {
	s := &Settings{Services: []ServiceConfig{
		{ID: "a", DisplayName: "A"},
		{ID: RelaySessionsServiceID, DisplayName: "Session Host"},
	}}
	s.SetServiceMenuHidden(RelaySessionsServiceID, true)
	if !s.Services[1].HideFromMenu {
		t.Fatal("session host row was not hidden")
	}
	if err := s.MoveService(RelaySessionsServiceID, 0); err != nil {
		t.Fatalf("MoveService(session host, 0): %v", err)
	}
	if got := svcOrderIDs(s); !slices.Equal(got, []string{RelaySessionsServiceID, "a"}) {
		t.Fatalf("order = %v", got)
	}
}

func TestServiceConfig_HideFromMenuJSON(t *testing.T) {
	shown, err := json.Marshal(ServiceConfig{ID: "a", DisplayName: "A"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(shown, []byte("hide_from_menu")) {
		t.Fatalf("a shown service must omit hide_from_menu, got %s", shown)
	}

	hidden, err := json.Marshal(ServiceConfig{ID: "a", DisplayName: "A", HideFromMenu: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(hidden, []byte(`"hide_from_menu":true`)) {
		t.Fatalf("a hidden service must carry hide_from_menu:true, got %s", hidden)
	}
	var back ServiceConfig
	if err := json.Unmarshal(hidden, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !back.HideFromMenu {
		t.Fatal("hide_from_menu did not round-trip")
	}

	var old ServiceConfig
	if err := json.Unmarshal([]byte(`{"id":"a","display_name":"A","command":"/bin/true"}`), &old); err != nil {
		t.Fatalf("unmarshal old record: %v", err)
	}
	if old.HideFromMenu {
		t.Fatal("a record without hide_from_menu must load as shown")
	}
}

// An existing settings.json predating the field loads with every service
// shown and in its stored order.
func TestSettingsStore_OldServicesLoadShownInOrder(t *testing.T) {
	home := mkEmptySandboxRelayHome(t)
	dir := filepath.Join(home, "cfg")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	raw := `{"services":[
		{"id":"zeta","display_name":"Zeta","command":"/bin/true"},
		{"id":"alpha","display_name":"Alpha","command":"/bin/true"},
		{"id":"mid","display_name":"Mid","command":"/bin/true"}
	]}`
	if err := os.WriteFile(sdSettingsPath(dir), []byte(raw), 0600); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}

	s := sealedSettingsStoreAt(dir).Get()
	if got, want := svcOrderIDs(s), []string{"zeta", "alpha", "mid"}; !slices.Equal(got, want) {
		t.Fatalf("loaded order = %v, want %v", got, want)
	}
	for _, svc := range s.Services {
		if svc.HideFromMenu {
			t.Fatalf("%s loaded hidden; an old record must load shown", svc.ID)
		}
	}
}

// Order and the hidden flag survive a restart: a fresh store over the same
// directory reads back what the first one wrote.
func TestSettingsStore_ServiceOrderAndHiddenPersistAcrossReload(t *testing.T) {
	dir := mkEmptySandboxRelayHome(t)
	store := sealedSettingsStoreAt(dir)
	if err := store.EnsureInitialized(); err != nil {
		t.Fatalf("EnsureInitialized: %v", err)
	}
	if err := store.With(func(s *Settings) {
		s.UpsertService(ServiceConfig{ID: "a", DisplayName: "A", Command: "/bin/true"})
		s.UpsertService(ServiceConfig{ID: "b", DisplayName: "B", Command: "/bin/true"})
		s.UpsertService(ServiceConfig{ID: "c", DisplayName: "C", Command: "/bin/true"})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var moveErr error
	if err := store.With(func(s *Settings) {
		moveErr = s.MoveService("c", 0)
		s.SetServiceMenuHidden("a", true)
	}); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	if moveErr != nil {
		t.Fatalf("MoveService: %v", moveErr)
	}

	reloaded := sealedSettingsStoreAt(dir).Get()
	if got, want := svcOrderIDs(reloaded), []string{"c", "a", "b"}; !slices.Equal(got, want) {
		t.Fatalf("reloaded order = %v, want %v", got, want)
	}
	for _, svc := range reloaded.Services {
		if svc.HideFromMenu != (svc.ID == "a") {
			t.Fatalf("reloaded %s HideFromMenu = %v", svc.ID, svc.HideFromMenu)
		}
	}

	raw := sdRead(t, dir)
	if n := bytes.Count(raw, []byte("hide_from_menu")); n != 1 {
		t.Fatalf("settings.json holds hide_from_menu %d times, want exactly once (only the hidden record):\n%s", n, raw)
	}
}
