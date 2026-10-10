// Package contract runs one scenario against fakerelay and against the real
// relay test build, and fails when the two answer differently.
//
// # Writing a scenario
//
// A scenario is a Spec (the world both targets start from) and a Body that
// drives one target. Check starts both targets in parallel subtests, runs Body
// once on each, then compares the two transcripts:
//
//	func TestProjectsList(t *testing.T) {
//		t.Parallel()
//		Check(t, Scenario{
//			Surface: Projects,
//			Spec: Spec{
//				Credentials: []harness.CredentialSpec{{Name: "ops", Classes: []string{"read"}}},
//				Projects:    []Project{{ID: "p_acme", Name: "Acme", Mode: "work"}},
//			},
//			Body: func(r *Run) {
//				r.HTTP("ops", "GET", "/api/projects", nil)
//			},
//		})
//	}
//
// Every call on Run that makes a request or reads relay state adds one entry
// to the transcript. Body decides what is compared by what it calls: Note
// records a derived value, and a call it does not make records nothing. A
// scenario must not depend on which target it runs on; Run.Target.Kind exists
// for the hooks, not for branching. Run.CLIWith is Run.CLI with a trace or
// stdin, and learns the ids a CLI that exits 0 with one JSON object prints.
//
// The Spec is rendered twice. The real target gets settings.json, the
// credentials, the presence map, fake MCPs and the ssh stub. The fake target
// gets a world.json built from the same Spec. The runner writes project files
// and git repositories to disk for both, so the two read the same bytes. A
// path under "{remote}" is replaced by each target's own remote root. The
// token "{instance}" is replaced by the instance directory; an empty console
// project path means <instance>/projects/<id>.
//
// Fields the Spec cannot express are fixed identically on both targets:
// every project allows all models and all console templates ("*"), and
// carries no MCP grant. A host named by a Project must be in Spec.Hosts. A
// fake MCP's display name on the real target is "Fake MCP <ID>", so MCP.Name
// defaults to that.
//
// # What is normalised
//
// Each entry is normalised per target before comparison, so only behaviour
// differs. Paths (instance, config, remote, home, tmp and bundle directories)
// become <DIR>, <CONFIG>, <REMOTE>, <HOME>, <TMP> and <BIN>. Ports and pids
// become <PORT> and <PID>. Generated ids (a create response's id, session and
// terminal ids, trace ids, credential ids, any UUID) become <ID:n>, numbered
// by first appearance, while ids written in the Spec stay as written. RFC 3339
// strings become <TS>, and a fixed list of time keys become <NUM>. 64-hex
// strings and planted tokens become <SECRET>. Host views, probes, host_status
// errors, ssh_argv, fs_event kinds, stats_update numbers, the ready file's
// fake-only keys and the audit rows only fakerelay writes get the rules in
// normalise.go. Terminal output is recorded by the scenario as
// {"echoed": bool} through Note, because fakerelay echoes by design.
//
// # The two rules that hold the normaliser steady
//
// Adding a key to the time or id lists needs a stated reason in the PR; the
// reviewer checks that the key masks no real difference. Order is compared
// exactly, except where a step uses WS.Expect, which skips frames it does not
// match.
//
// # Waits
//
// Nothing here sleeps. Both target subtests finish before Compare runs;
// WS.Until and WS.Expect read frames with a deadline; Run.Event is the exit of
// "relay logs --follow"; SetHostStatus on the real target is the host_status
// frame on the hook's own /ws/files connection.
package contract
