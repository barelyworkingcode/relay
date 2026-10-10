package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// FakeOptions shape one fakerelay instance (docs/fakerelay.md, "World spec").
type FakeOptions struct {
	World        map[string]json.RawMessage // world.json top-level keys
	Credentials  []CredentialSpec           // the harness mints token and id and appends them to the world's credentials
	Presence     map[string]Outcome
	BootDeadline time.Duration
	PrepareDir   func(dir string) // runs after world.json is written, before serve starts
}

// StartFake boots a fakerelay on its own dir and returns the same Instance a
// real boot returns. Boot waits on the first stdout line, as serve's does.
func StartFake(t *testing.T, o FakeOptions) *Instance {
	t.Helper()
	i := newInstanceFor(t, Options{
		Credentials:      o.Credentials,
		Presence:         o.Presence,
		BootDeadline:     o.BootDeadline,
		PrepareConfigDir: o.PrepareDir,
	}, &o)
	t.Cleanup(i.cleanup)
	if res, ok := i.boot(); !ok {
		t.Fatalf("fakerelay did not become ready: exit %d\nstderr: %s", res.Code, tailString(res.Stderr, 2000))
	}
	return i
}

// IsFake reports whether the instance is a fakerelay.
func (i *Instance) IsFake() bool { return i.fake }

// Ctl runs `fakerelay ctl` against the instance and returns its result
// whatever the exit code. It fails t on a real instance.
func (i *Instance) Ctl(args ...string) Result {
	i.t.Helper()
	if !i.fake {
		i.t.Fatalf("Ctl %v: ctl is a fakerelay verb and this is a real instance", args)
	}
	p := startProc(i.t, procSpec{
		bin: i.bin, args: append([]string{"--config-dir", i.ConfigDir, "ctl"}, args...),
		env: i.env, dir: i.Dir, deadline: defaultCLIDeadline,
	})
	p.onResult = i.noteStderr
	return p.Wait()
}

// RemoteRoot is the directory a host's remote side lives in: <Dir>/remote,
// created on every call, for both kinds of instance.
func (i *Instance) RemoteRoot() string {
	i.t.Helper()
	root := filepath.Join(i.Dir, "remote")
	if err := os.MkdirAll(root, 0o700); err != nil {
		i.t.Fatalf("creating %s: %v", root, err)
	}
	return root
}

// writeWorld writes world.json: the caller's keys, then the harness's
// credentials, presence and agent templates.
func (i *Instance) writeWorld(fo *FakeOptions) {
	t := i.t
	t.Helper()
	world := map[string]json.RawMessage{"schema": json.RawMessage("1")}
	appended := map[string][]json.RawMessage{}
	for k, v := range fo.World {
		switch k {
		case "credentials", "terminal_templates":
			var arr []json.RawMessage
			if err := json.Unmarshal(v, &arr); err != nil {
				t.Fatalf("FakeOptions.World[%q] must be a JSON array: %v", k, err)
			}
			appended[k] = arr
		default:
			world[k] = v
		}
	}
	appended["terminal_templates"] = i.agentTemplates(appended["terminal_templates"])
	for _, c := range fo.Credentials {
		i.newCredential(c) // records the minted token and id in i.creds
		minted := i.creds[c.Name]
		rec := map[string]any{"id": minted.ID, "name": c.Name, "classes": minted.Classes, "token": minted.Token}
		if !c.Expires.IsZero() {
			rec["expires"] = c.Expires.UTC().Format(time.RFC3339)
		}
		raw, _ := json.Marshal(rec)
		appended["credentials"] = append(appended["credentials"], raw)
	}
	for k, arr := range appended {
		raw, err := json.Marshal(arr)
		if err != nil {
			t.Fatalf("encoding %s: %v", k, err)
		}
		world[k] = raw
	}
	if fo.Presence != nil {
		raw, _ := json.Marshal(fo.Presence)
		world["presence"] = raw
	}
	data, err := json.MarshalIndent(world, "", "  ")
	if err != nil {
		t.Fatalf("encoding world.json: %v", err)
	}
	writeFileAtomic(t, filepath.Join(i.ConfigDir, "world.json"), data)
}
