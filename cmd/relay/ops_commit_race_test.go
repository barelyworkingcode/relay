package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
)

type ocrDeps struct {
	store *config.FileSettingsStore
	hook  *odwHookStore
	queue *config.CommandQueue
	block func()
}

type ocrVerify func(t *testing.T, stored *config.Settings)

// ocrReloadBlocker holds a config save at its restart: the file is already
// written, so the save has committed and only the process side is pending.
type ocrReloadBlocker struct {
	*cfgSaveManager
	block func()
}

func (m *ocrReloadBlocker) Reload(id string, c *config.ServiceConfig) error {
	m.block()
	return m.cfgSaveManager.Reload(id, c)
}

func ocrCredentialOps(t *testing.T, d ocrDeps) *CredentialOps {
	return &CredentialOps{Store: d.hook, Queue: d.queue, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
}

func ocrServiceOps(t *testing.T, d ocrDeps) *ServiceOps {
	return &ServiceOps{Store: d.hook, Registry: noopServiceManager{}, Queue: d.queue, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
}

func ocrLoginOps(t *testing.T, d ocrDeps) *LoginOps {
	return &LoginOps{Store: d.hook, Queue: d.queue, Gate: allowGate(t), Audit: enabledIssuanceRecorder(t)}
}

func ocrWantStoredService(t *testing.T, stored *config.Settings, cfg config.ServiceConfig, err error, id, name string) {
	t.Helper()
	if err != nil || cfg.ID != id || cfg.DisplayName != name {
		t.Fatalf("got (id %q, name %q, err %v), want (%q, %q, nil)", cfg.ID, cfg.DisplayName, err, id, name)
	}
	if got, _ := config.FindServiceByID(stored, id); got == nil || got.DisplayName != name {
		t.Fatalf("stored service = %+v, want %q named %q", got, id, name)
	}
}

// A caller that cancels after its step is admitted must still be told what
// the step committed; for a mint, an error in its place drops the only copy
// of a secret that is already stored.
func TestOps_CommittedResultSurvivesCallerCancel(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, d ocrDeps) func(ctx context.Context) ocrVerify
	}{
		{"CredentialOps.Mint", func(t *testing.T, d ocrDeps) func(context.Context) ocrVerify {
			ops := ocrCredentialOps(t, d)
			return func(ctx context.Context) ocrVerify {
				cred, plaintext, err := ops.Mint(ctx, credentialMintRequest{Name: "Acme CLI", Classes: []string{"read"}}, auditViaCLI, "")
				return func(t *testing.T, stored *config.Settings) {
					if err != nil {
						for _, c := range stored.APICredentials {
							if c.Name == "Acme CLI" {
								t.Fatalf("Mint err = %v, but credential %s was stored: the caller lost its only plaintext", err, c.ID)
							}
						}
						t.Fatalf("Mint err = %v, want the committed credential", err)
					}
					if got := authenticateAPICredential(stored, plaintext); got == nil || got.ID != cred.ID {
						t.Fatalf("returned plaintext authenticates as %+v, want the returned credential %q", got, cred.ID)
					}
				}
			}
		}},
		{"CredentialOps.Revoke", func(t *testing.T, d ocrDeps) func(context.Context) ocrVerify {
			seeded, _, err := mintAPICredential(d.store, credentialMintRequest{Name: "Acme CLI", Classes: []string{"read"}})
			assertNoErr(t, err, "seed credential")
			ops := ocrCredentialOps(t, d)
			return func(ctx context.Context) ocrVerify {
				removed, err := ops.Revoke(ctx, seeded.ID, auditViaCLI, "")
				return func(t *testing.T, stored *config.Settings) {
					if err != nil || removed.ID != seeded.ID {
						t.Fatalf("Revoke = (id %q, err %v), want (%q, nil)", removed.ID, err, seeded.ID)
					}
					if findAPICredential(stored, seeded.ID) != nil {
						t.Fatalf("credential %q still stored after a successful revoke", seeded.ID)
					}
				}
			}
		}},
		{"ServiceOps.Register", func(t *testing.T, d ocrDeps) func(context.Context) ocrVerify {
			ops := ocrServiceOps(t, d)
			return func(ctx context.Context) ocrVerify {
				cfg, err := ops.Register(ctx, serviceFields{ID: "acme-agent", DisplayName: "Acme Agent", Command: "/bin/true"}, auditViaCLI, "")
				return func(t *testing.T, stored *config.Settings) {
					ocrWantStoredService(t, stored, cfg, err, "acme-agent", "Acme Agent")
				}
			}
		}},
		{"ServiceOps.Create", func(t *testing.T, d ocrDeps) func(context.Context) ocrVerify {
			ops := ocrServiceOps(t, d)
			return func(ctx context.Context) ocrVerify {
				cfg, err := ops.Create(ctx, serviceFields{DisplayName: "Acme Worker", Command: "/bin/true"}, auditViaCLI, "")
				return func(t *testing.T, stored *config.Settings) {
					ocrWantStoredService(t, stored, cfg, err, "acme-worker", "Acme Worker")
				}
			}
		}},
		{"ServiceOps.Update", func(t *testing.T, d ocrDeps) func(context.Context) ocrVerify {
			odwSeedService(t, d.store)
			ops := ocrServiceOps(t, d)
			return func(ctx context.Context) ocrVerify {
				cfg, err := ops.Update(ctx, "keeper", serviceFields{DisplayName: "Keeper Renamed", Command: "/bin/true"}, auditViaCLI, "")
				return func(t *testing.T, stored *config.Settings) {
					ocrWantStoredService(t, stored, cfg, err, "keeper", "Keeper Renamed")
				}
			}
		}},
		{"ServiceOps.SaveConfigFile", func(t *testing.T, d ocrDeps) func(context.Context) ocrVerify {
			root := t.TempDir()
			path := filepath.Join(root, "settings.json")
			writeFile(t, path, `{"a":"seed"}`)
			sorSeedService(t, d.store, config.ServiceConfig{ID: "svc", DisplayName: "Svc", Command: "/bin/x", WorkingDir: root})
			m := configManifest(path)
			m.Config.ApplyMode = bridge.ConfigApplyRestart
			reg := NewEnhancedServiceRegistry(nil)
			assertNoErr(t, reg.RegisterManifest("svc", "/sock", "tok", m), "register manifest")
			mgr := &ocrReloadBlocker{cfgSaveManager: &cfgSaveManager{running: true, path: path}, block: d.block}
			ops := &ServiceOps{Store: d.store, Registry: mgr, Enhanced: reg, Queue: d.queue, Gate: allowGate(t), Issuance: enabledIssuanceRecorder(t)}
			const text = `{"a":"saved"}`
			return func(ctx context.Context) ocrVerify {
				res, err := ops.SaveConfigFile(ctx, "svc", text)
				return func(t *testing.T, _ *config.Settings) {
					if err != nil || !res.Restarted {
						t.Fatalf("SaveConfigFile = (restarted %v, err %v), want (true, nil)", res.Restarted, err)
					}
					if got, readErr := os.ReadFile(path); readErr != nil || string(got) != text {
						t.Fatalf("config file = %q (read err %v), want %q", got, readErr, text)
					}
				}
			}
		}},
		{"LoginOps.MintBootstrap", func(t *testing.T, d ocrDeps) func(context.Context) ocrVerify {
			ops := ocrLoginOps(t, d)
			return func(ctx context.Context) ocrVerify {
				view, err := ops.MintBootstrap(ctx, auditViaCLI)
				return func(t *testing.T, stored *config.Settings) {
					if err != nil {
						if stored.LoginBootstrap != nil {
							t.Fatalf("MintBootstrap err = %v, but a login code was stored: the caller lost its only plaintext", err)
						}
						t.Fatalf("MintBootstrap err = %v, want the committed code", err)
					}
					if view.Code == "" || stored.LoginBootstrap == nil || stored.LoginBootstrap.Hash != config.HashToken(view.Code) {
						t.Fatalf("returned code %q is not the stored anchor %+v", view.Code, stored.LoginBootstrap)
					}
				}
			}
		}},
		{"LoginOps.RevokePasskey", func(t *testing.T, d ocrDeps) func(context.Context) ocrVerify {
			const id = "pk-acme-1"
			assertNoErr(t, d.store.With(func(s *config.Settings) {
				s.Passkeys = append(s.Passkeys, config.Passkey{ID: id, Name: "Acme key"})
			}), "seed passkey")
			ops := ocrLoginOps(t, d)
			return func(ctx context.Context) ocrVerify {
				removed, err := ops.RevokePasskey(ctx, id)
				return func(t *testing.T, stored *config.Settings) {
					if err != nil || removed.ID != id {
						t.Fatalf("RevokePasskey = (id %q, err %v), want (%q, nil)", removed.ID, err, id)
					}
					for _, p := range stored.Passkeys {
						if p.ID == id {
							t.Fatalf("passkey %q still stored after a successful revoke", id)
						}
					}
				}
			}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, store := odwSandbox(t)

			queue, err := config.NewCommandQueue(1)
			assertNoErr(t, err, "NewCommandQueue")
			t.Cleanup(func() { _ = queue.Shutdown(context.Background()) })

			entered := make(chan struct{})
			release := make(chan struct{})
			var enterOnce, releaseOnce sync.Once
			releaseWrite := func() { releaseOnce.Do(func() { close(release) }) }
			// Registered after the queue's cleanup so it runs first: Shutdown
			// waits for the blocked step, which ignores its ctx.
			t.Cleanup(releaseWrite)
			block := func() {
				enterOnce.Do(func() { close(entered) })
				<-release
			}

			d := ocrDeps{store: store, hook: &odwHookStore{FileSettingsStore: store, preWrite: block}, queue: queue, block: block}
			run := tc.setup(t, d)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan ocrVerify, 1)
			go func() { done <- run(ctx) }()

			select {
			case <-entered:
			case v := <-done:
				v(t, store.Get())
				t.Fatal("returned before its step reached the commit point, so the fixture proves nothing")
			}
			cancel()
			releaseWrite()
			check := <-done

			assertNoErr(t, queue.Do(context.Background(), func(context.Context) error { return nil }), "drain queue")
			check(t, store.Get())
		})
	}
}
