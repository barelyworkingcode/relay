package main

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/project"
)

// A caller that cancels after its step is admitted must still be told what
// the step committed: the step runs to completion regardless, so an error in
// its place misreports stored state, and for a rotation it drops the only
// copy of the new token.
func TestProjectOps_CommittedResultSurvivesCallerCancel(t *testing.T) {
	type verify func(t *testing.T, stored *config.Settings)
	cases := []struct {
		name string
		seed func(t *testing.T, store config.SettingsStore) config.Project
		run  func(ctx context.Context, ops *ProjectOps, seeded config.Project) verify
	}{
		{
			name: "Create",
			seed: func(t *testing.T, _ config.SettingsStore) config.Project { return config.Project{Path: t.TempDir()} },
			run: func(ctx context.Context, ops *ProjectOps, seeded config.Project) verify {
				created, err := ops.Create(ctx, project.CreateFields{Name: "Acme", Path: seeded.Path}, nil, auditViaCLI, "")
				return func(t *testing.T, stored *config.Settings) {
					if err != nil {
						t.Fatalf("Create err = %v, want the committed project", err)
					}
					got, _ := config.FindProjectByID(stored, created.ID)
					if created.ID == "" || got == nil || got.Name != "Acme" {
						t.Fatalf("Create returned %q, stored %+v; want the returned project stored as Acme", created.ID, got)
					}
				}
			},
		},
		{
			name: "Update",
			seed: func(t *testing.T, store config.SettingsStore) config.Project {
				return mkStoreProject(t, store, config.ProjectKindLocal, "Acme", t.TempDir())
			},
			run: func(ctx context.Context, ops *ProjectOps, seeded config.Project) verify {
				name := "Acme Renamed"
				updated, found, err := ops.Update(ctx, seeded.ID, project.UpdateFields{Name: &name},
					func() project.McpSurfaces { return nil }, auditViaCLI, "")
				return func(t *testing.T, stored *config.Settings) {
					if err != nil || !found || updated.Name != name {
						t.Fatalf("Update = (name %q, found %v, err %v), want (%q, true, nil)", updated.Name, found, err, name)
					}
					if got, _ := config.FindProjectByID(stored, seeded.ID); got == nil || got.Name != name {
						t.Fatalf("stored project = %+v, want name %q", got, name)
					}
				}
			},
		},
		{
			name: "RotateToken",
			seed: func(t *testing.T, store config.SettingsStore) config.Project {
				return mkStoreProject(t, store, config.ProjectKindLocal, "Acme", t.TempDir())
			},
			run: func(ctx context.Context, ops *ProjectOps, seeded config.Project) verify {
				token, found, err := ops.RotateToken(ctx, seeded.ID, auditViaCLI, "")
				return func(t *testing.T, stored *config.Settings) {
					got, _ := config.FindProjectByID(stored, seeded.ID)
					if got == nil {
						t.Fatal("project vanished")
					}
					if err != nil {
						if got.TokenHash != seeded.TokenHash {
							t.Fatalf("RotateToken err = %v, but a new token was stored: the caller lost the only copy", err)
						}
						t.Fatalf("RotateToken err = %v, want the committed token", err)
					}
					if !found || token == "" {
						t.Fatalf("RotateToken = (%q, found %v), want a token", token, found)
					}
					auth, authErr := stored.AuthenticateProject(token)
					if authErr != nil || auth == nil || got.TokenHash == seeded.TokenHash {
						t.Fatalf("returned token does not authenticate as the stored rotation (auth err %v)", authErr)
					}
				}
			},
		},
		{
			name: "NarrowForEnrolment",
			seed: func(t *testing.T, store config.SettingsStore) config.Project {
				return pgwSeedGrantProject(t, store, config.ProjectKindRemote, "p1")
			},
			run: func(ctx context.Context, ops *ProjectOps, seeded config.Project) verify {
				ids := []string{"macmcp"}
				caller := bridge.RemoteCaller{ClientID: "acme-client", Fingerprint: "sha256:" + strings.Repeat("a", 64)}
				updated, changed, err := ops.NarrowForEnrolment(ctx, seeded.ID, project.NarrowFields{AllowedMcpIDs: &ids}, caller,
					func() project.McpSurfaces { return project.McpSurfaces{"macmcp": macmcpSurface()} })
				return func(t *testing.T, stored *config.Settings) {
					if err != nil || len(changed) == 0 || !slices.Equal(updated.AllowedMcpIDs, ids) {
						t.Fatalf("NarrowForEnrolment = (mcps %v, changed %v, err %v), want (%v, non-empty, nil)", updated.AllowedMcpIDs, changed, err, ids)
					}
					if got, _ := config.FindProjectByID(stored, seeded.ID); got == nil || !slices.Equal(got.AllowedMcpIDs, ids) {
						t.Fatalf("stored project = %+v, want allowed_mcp_ids %v", got, ids)
					}
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, store := odwSandbox(t)
			seeded := tc.seed(t, store)

			queue, err := config.NewCommandQueue(1)
			assertNoErr(t, err, "NewCommandQueue")
			t.Cleanup(func() { _ = queue.Shutdown(context.Background()) })

			entered := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseWrite := func() { releaseOnce.Do(func() { close(release) }) }
			// Registered after the queue's cleanup so it runs first: Shutdown
			// waits for the blocked step, which ignores its ctx.
			t.Cleanup(releaseWrite)

			hook := &odwHookStore{FileSettingsStore: store, preWrite: func() {
				close(entered)
				<-release
			}}
			ops := &ProjectOps{Store: hook, Queue: queue, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan verify, 1)
			go func() { done <- tc.run(ctx, ops, seeded) }()

			select {
			case <-entered:
			case v := <-done:
				v(t, store.Get())
				t.Fatal("returned before its step reached the store, so the fixture proves nothing")
			}
			cancel()
			releaseWrite()
			check := <-done

			assertNoErr(t, queue.Do(context.Background(), func(context.Context) error { return nil }), "drain queue")
			check(t, store.Get())
		})
	}
}
