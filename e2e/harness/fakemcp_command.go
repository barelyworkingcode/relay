package harness

import (
	"encoding/json"
	"os"
)

// FakeMCPCommand writes spec's catalogue and returns the command and args that
// run it as a stdio fake, without attaching it to settings.json, so a test can
// register it through `relay mcp register`. FakeMCPCalls(spec.ID) reads its log.
func (i *Instance) FakeMCPCommand(spec FakeMCPSpec) (command string, args []string) {
	t := i.t
	t.Helper()
	if spec.ID == "" {
		t.Fatalf("FakeMCPSpec needs an ID")
	}
	cat, err := json.Marshal(spec.Catalogue)
	if err != nil {
		t.Fatalf("encoding the catalogue of fake MCP %s: %v", spec.ID, err)
	}
	catPath := i.fakePath(spec.ID + ".catalogue.json")
	if err := os.WriteFile(catPath, cat, 0o600); err != nil {
		t.Fatalf("writing %s: %v", catPath, err)
	}
	spec.Transport = "stdio"
	f := &fakeMCP{spec: spec, logPath: i.fakePath(spec.ID + ".calls.jsonl")}
	i.fakeMCPs = append(i.fakeMCPs, f)
	return bundle.FakeMCP, []string{"--catalogue", catPath, "--call-log", f.logPath, "--transport", "stdio"}
}
