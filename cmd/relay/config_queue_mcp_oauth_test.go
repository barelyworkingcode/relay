package main

import (
	"context"
	"errors"
	"testing"

	"github.com/barelyworkingcode/relay/internal/config"
)

func oauthStateFor(store config.SettingsStore, id string) *config.OAuthState {
	mcp, _ := config.FindExternalMcpByID(store.Get(), id)
	if mcp == nil {
		return nil
	}
	return mcp.OAuthState
}

func TestMcpOpsPersistOAuthStateWaitsBehindQueuedCommandAndLandsInOrder(t *testing.T) {
	store := eveOpsStore(t)
	assertNoErr(t, store.With(func(s *config.Settings) {
		s.ExternalMcps = []config.ExternalMcp{{ID: "remote-mcp", DisplayName: "Remote"}}
	}), "seed mcp")
	queue := newLoginQueue(t, 4)
	ops := &McpOps{Store: store, Ctx: context.Background(), Queue: queue}

	releaseBlocker := queueBlocker(t, queue)
	persisted := make(chan error, 1)
	go func() {
		persisted <- ops.PersistOAuthState("remote-mcp", &config.OAuthState{ClientID: "refreshed"})
	}()
	waitForMutationAdmission(t, queue)
	assertMutationStillWaiting(t, persisted)
	if oauthStateFor(store, "remote-mcp") != nil {
		t.Fatal("refreshed OAuth state persisted before the queue admitted it")
	}

	seen := make(chan string, 1)
	go func() {
		_ = queue.Do(context.Background(), func(context.Context) error {
			if st := oauthStateFor(store, "remote-mcp"); st != nil {
				seen <- st.ClientID
			} else {
				seen <- ""
			}
			return nil
		})
	}()
	waitForPending(t, queue, 2)
	releaseBlocker()

	assertNoErr(t, <-persisted, "PersistOAuthState")
	if got := <-seen; got != "refreshed" {
		t.Fatalf("a command queued after the refresh saw client id %q, want %q", got, "refreshed")
	}
}

func TestMcpOpsPersistOAuthStateDropsARemovedMcp(t *testing.T) {
	store := eveOpsStore(t)
	ops := &McpOps{Store: store, Ctx: context.Background()}
	if err := ops.PersistOAuthState("gone", &config.OAuthState{ClientID: "x"}); !errors.Is(err, errMcpNotFound) {
		t.Fatalf("err = %v, want errMcpNotFound", err)
	}
}
