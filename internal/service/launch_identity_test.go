package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/peertoken"
)

func beginService(t *testing.T, table *Launches, name string, frontend bool) (string, *Launch) {
	t.Helper()
	secret, l, err := table.Begin(Identity{Kind: IdentityKindService, Name: name, Capabilities: capsFor(frontend)})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return secret, l
}

func TestLaunches_BeginIssuesA64HexSecret(t *testing.T) {
	secret, _ := beginService(t, NewLaunches(), "svc", false)
	if !isLaunchSecretShape(secret) {
		t.Fatalf("secret %q is not 64 lowercase hex characters", secret)
	}
}

func TestLaunches_BindRecordsTheProcessAndLookupFindsIt(t *testing.T) {
	table := NewLaunches()
	secret, _ := beginService(t, table, "svc", false)
	peer := peertoken.ForProcessForTest(500, 9)

	id, err := table.Bind("svc", secret, peer)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if id.Kind != IdentityKindService || id.Name != "svc" || !id.Allows(OpRegisterManifest) || id.Allows(OpFrontendSocket) {
		t.Fatalf("bound identity = %+v", id)
	}
	got, ok := table.Lookup(peer)
	if !ok || got.Process != peer.Process() {
		t.Fatalf("Lookup = %+v, %v", got, ok)
	}
}

func TestLaunches_AReusedPidWithANewPidversionIsNotTheService(t *testing.T) {
	table := NewLaunches()
	secret, _ := beginService(t, table, "svc", false)
	if _, err := table.Bind("svc", secret, peertoken.ForProcessForTest(500, 9)); err != nil {
		t.Fatal(err)
	}
	if _, ok := table.Lookup(peertoken.ForProcessForTest(500, 10)); ok {
		t.Fatal("a different pidversion under the same pid inherited the identity")
	}
}

func TestLaunches_WrongSecretIsRefusedAndDoesNotSpendTheLaunch(t *testing.T) {
	table := NewLaunches()
	secret, _ := beginService(t, table, "svc", false)
	forged := strings.Repeat("a", LaunchSecretHexLen)

	_, err := table.Bind("svc", forged, peertoken.ForProcessForTest(600, 1))
	if !errors.Is(err, ErrHelloRefused) {
		t.Fatalf("forged Bind: err = %v", err)
	}
	if strings.Contains(err.Error(), forged) || strings.Contains(err.Error(), secret) {
		t.Fatalf("refusal echoed a secret: %v", err)
	}
	if _, ok := table.Lookup(peertoken.ForProcessForTest(600, 1)); ok {
		t.Fatal("a forged Hello bound an identity")
	}
	if _, err := table.Bind("svc", secret, peertoken.ForProcessForTest(700, 1)); err != nil {
		t.Fatalf("the real secret was refused after a forgery: %v", err)
	}
}

func TestLaunches_SecondBindWithTheRightSecretIsRefused(t *testing.T) {
	table := NewLaunches()
	secret, _ := beginService(t, table, "svc", false)
	first := peertoken.ForProcessForTest(800, 1)
	if _, err := table.Bind("svc", secret, first); err != nil {
		t.Fatal(err)
	}
	second := peertoken.ForProcessForTest(801, 1)
	if _, err := table.Bind("svc", secret, second); !errors.Is(err, ErrHelloRefused) {
		t.Fatalf("second Bind: err = %v, want refused", err)
	}
	if _, ok := table.Lookup(second); ok {
		t.Fatal("the second presenter gained the identity")
	}
	if _, ok := table.Lookup(first); !ok {
		t.Fatal("the second attempt disturbed the first binding")
	}
}

func TestLaunches_RefusesMalformedSecretsUnknownNamesAndInvalidPeers(t *testing.T) {
	table := NewLaunches()
	secret, _ := beginService(t, table, "svc", false)
	peer := peertoken.ForProcessForTest(900, 1)
	for name, tc := range map[string]struct {
		name, secret string
		peer         peertoken.Token
	}{
		"uppercase":    {"svc", strings.ToUpper(secret), peer},
		"short":        {"svc", secret[:63], peer},
		"unknown name": {"other", secret, peer},
		"no peer":      {"svc", secret, peertoken.Token{}},
	} {
		if _, err := table.Bind(tc.name, tc.secret, tc.peer); !errors.Is(err, ErrHelloRefused) {
			t.Errorf("%s: err = %v, want refused", name, err)
		}
	}
}

func TestLaunches_OneProcessHoldsAtMostOneIdentity(t *testing.T) {
	table := NewLaunches()
	s1, _ := beginService(t, table, "one", false)
	s2, _ := beginService(t, table, "two", true)
	peer := peertoken.ForProcessForTest(1000, 1)
	if _, err := table.Bind("one", s1, peer); err != nil {
		t.Fatal(err)
	}
	if _, err := table.Bind("two", s2, peer); !errors.Is(err, ErrHelloRefused) {
		t.Fatalf("a second identity for the same process: err = %v", err)
	}
}

func TestLaunches_EndClearsTheIdentity(t *testing.T) {
	table := NewLaunches()
	secret, l := beginService(t, table, "svc", false)
	peer := peertoken.ForProcessForTest(1100, 1)
	if _, err := table.Bind("svc", secret, peer); err != nil {
		t.Fatal(err)
	}
	l.End()
	if _, ok := table.Lookup(peer); ok {
		t.Fatal("identity outlived its launch")
	}
	if _, ok := table.Bound("svc"); ok {
		t.Fatal("Bound still reports an ended launch")
	}
	if _, err := table.Bind("svc", secret, peertoken.ForProcessForTest(1101, 1)); !errors.Is(err, ErrHelloRefused) {
		t.Fatalf("an ended launch's secret still binds: %v", err)
	}
	l.End()
}

func TestLaunches_ARestartReplacesThePreviousLaunch(t *testing.T) {
	table := NewLaunches()
	oldSecret, oldLaunch := beginService(t, table, "svc", false)
	oldPeer := peertoken.ForProcessForTest(1200, 1)
	if _, err := table.Bind("svc", oldSecret, oldPeer); err != nil {
		t.Fatal(err)
	}
	newSecret, _ := beginService(t, table, "svc", false)
	if _, ok := table.Lookup(oldPeer); ok {
		t.Fatal("the previous launch's identity survived a new Begin")
	}
	if _, err := table.Bind("svc", oldSecret, oldPeer); err == nil {
		t.Fatal("the previous launch's secret binds the new launch")
	}
	newPeer := peertoken.ForProcessForTest(1201, 1)
	if _, err := table.Bind("svc", newSecret, newPeer); err != nil {
		t.Fatal(err)
	}
	oldLaunch.End()
	if _, ok := table.Lookup(newPeer); !ok {
		t.Fatal("ending the replaced launch cleared the new launch's identity")
	}
}
