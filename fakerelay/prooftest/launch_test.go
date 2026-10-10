package prooftest

import (
	"path/filepath"
	"strings"
	"testing"
)

func launchWorld(mode string, caps ...string) func(string) any {
	return func(dir string) any {
		svc := service(dir, "probe", mode, "probe", caps...)
		svc["env"].(J)["RELAY_SERVICE_ID"] = "spoofed"
		return J{"schema": 1, "credentials": creds(), "services": []J{svc}}
	}
}

// leaky is fakerelay's own environment: none of it may reach the child.
var leaky = []string{"RELAY_SERVICE_TOKEN=leak", "RELAY_MCP_TOKEN=leak", "RELAY_FRONTEND_TOKEN=leak", "RELAY_LAUNCH_FD=9"}

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	must(t, err, "eval "+p)
	return r
}

// Criteria: fd 3 launch with the one-shot secret; launch env; Hello rules; identity on the frontend socket.
func TestLaunchIdentity(t *testing.T) {
	t.Parallel()

	t.Run("a frontend service gets the secret on fd 3 and a clean env", func(t *testing.T) {
		t.Parallel()
		in := startInstance(t, launchWorld("normal", "frontend", "manifest"), leaky...)
		in.waitRegistered(t)
		rep := in.report(t, "probe")
		if !rep.SecretHex64 || !rep.FD3EOF || !rep.HelloOK || !rep.AuthOK {
			t.Errorf("launch report: %+v", rep)
		}
		eq(t, rep.LaunchFD, "3", "RELAY_LAUNCH_FD")
		eq(t, len(rep.Leaked), 0, "token variables in the child env")
		eq(t, rep.ServiceID, "probe", "RELAY_SERVICE_ID wins over the record env")
		eq(t, rep.FrontendSocket, in.ready.Sockets.Frontend, "RELAY_FRONTEND_SOCKET")
		eq(t, rep.BridgeSocket, in.ready.Sockets.Bridge, "RELAY_BRIDGE_SOCKET")
		eq(t, realPath(t, rep.ConfigDir), realPath(t, in.dir), "RELAY_CONFIG_DIR")
		eq(t, rep.ProjectsStatus, 200, "the identity's own request with no Authorization")

		// This test process holds no launch identity.
		r, err := send(t.Context(), in.sockClient(), "http://relay", "GET", "/api/projects", "", nil)
		must(t, err, "no-header request")
		r.is(t, 401)

		nope := in.bridgeCall(t, J{"type": "Hello", "name": "nosuch", "token": strings.Repeat("b", 64)})
		eq(t, [3]any{nope["type"], nope["code"], nope["message"]}, [3]any{"Error", -32001.0, "hello refused"}, "Hello without a launch")
		unk := in.bridgeCall(t, J{"type": "Foo"})
		eq(t, [3]any{unk["type"], unk["code"], unk["message"]}, [3]any{"Error", -32601.0, "unknown request type: Foo"}, "unknown type")
		reg := in.bridgeCall(t, J{"type": "RegisterManifest", "arguments": J{"serviceId": "probe"}})
		eq(t, [2]any{reg["type"], reg["code"]}, [2]any{"Error", -32001.0}, "RegisterManifest without an identity")
	})

	t.Run("a service without frontend gets no socket variable and 401", func(t *testing.T) {
		t.Parallel()
		in := startInstance(t, launchWorld("normal", "manifest"))
		in.waitRegistered(t)
		rep := in.report(t, "probe")
		eq(t, rep.FrontendEnvSet, false, "RELAY_FRONTEND_SOCKET set")
		eq(t, rep.ProjectsStatus, 401, "identity without frontend")
	})

	t.Run("a wrong secret does not spend the launch", func(t *testing.T) {
		t.Parallel()
		in := startInstance(t, launchWorld("wrongsecret", "frontend", "manifest"))
		in.waitRegistered(t)
		rep := in.report(t, "probe")
		eq(t, [2]bool{rep.WrongSecretRefused, rep.HelloOK}, [2]bool{true, true}, "wrong then right secret")
	})

	t.Run("a second Hello is refused on the same and on another connection", func(t *testing.T) {
		t.Parallel()
		in := startInstance(t, launchWorld("twice", "frontend", "manifest"))
		in.waitRegistered(t)
		rep := in.report(t, "probe")
		eq(t, [3]bool{rep.HelloOK, rep.SecondHelloRefused, rep.OtherConnRefused}, [3]bool{true, true, true}, "repeat Hello")
	})

	t.Run("service restart mints a new launch and stops the old process", func(t *testing.T) {
		t.Parallel()
		in := startInstance(t, launchWorld("normal", "frontend", "manifest"))
		first := in.waitRegistered(t)
		old := in.report(t, "probe")
		out, se, code := in.cli(t, "service", "restart", "--id", "probe")
		eq(t, code, 0, "service restart exit: "+out+se)
		in.waitRegistered(t, "--since", after(first))
		fresh := in.report(t, "probe")
		if fresh.PID == old.PID || !fresh.HelloOK || !fresh.SecretHex64 {
			t.Errorf("restart did not give a new launch: old %+v new %+v", old, fresh)
		}
		in.waitProbeGone(t, filepath.Join(in.dir, "probe-"+itoa(old.PID)+".lock"))
		out, _, code = in.cli(t, "service", "list")
		if code != 0 || !strings.Contains(out, "probe") {
			t.Errorf("service list: exit %d %q", code, out)
		}
	})
}
