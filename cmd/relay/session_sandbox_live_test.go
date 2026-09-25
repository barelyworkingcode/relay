//go:build live

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/barelyworkingcode/relay/internal/control"
)

// The fixture sits under /private/tmp, outside TMPDIR and
// DARWIN_USER_TEMP_DIR, so no session temp grant covers it by accident.
func TestLive_ProviderOutsideEveryTemplateStartsUnderSandbox(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("no /usr/bin/sandbox-exec")
	}
	root, err := os.MkdirTemp("/private/tmp", "relay-live-provider-")
	if err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	for _, tmp := range []string{os.TempDir(), darwinUserTempDir()} {
		if tmp != "" && strings.HasPrefix(root, sandboxRealPath(t, tmp)) {
			t.Fatalf("fixture %s lies under a session temp grant %s", root, tmp)
		}
	}

	tempDir := filepath.Join(root, "tmp", "claude-"+strconv.Itoa(os.Getuid()))
	buildTree(t, root, map[string]string{
		"tmp/":                          "",
		"bin/claude":                    "->../lib/node_modules/@acme/cli/cli.sh",
		"lib/node_modules/dep/data.txt": "data\n",
		"lib/node_modules/@acme/cli/cli.sh": "#!/bin/sh\nset -e\n" +
			"read -r data < \"$ROOT/lib/node_modules/dep/data.txt\"\n" +
			"echo probe > \"" + tempDir + "/probe\"\n" +
			"echo ok\n",
	})
	link := filepath.Join(root, "bin", "claude")
	setSeam(t, &providerBinary, func(string) string { return link })
	setSeam(t, &claudeTempRoot, filepath.Join(root, "tmp"))

	store := newLaunchTestStore(t)
	setKindTemplates(t, store)
	proj := addLaunchTestProject(t, store, nil)
	result, _ := launchWithSandbox(t, LaunchRequest{
		Caller: bearerCaller(control.ClassExecute), ProjectID: proj.ID, Kind: KindClaude,
	}, store)

	cmd := exec.Command("/usr/bin/sandbox-exec", "-f", sandboxProfilePath(result), "--", link)
	cmd.Dir = proj.Path
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("provider under the sandbox: err=%v output=%q", err, out)
	}
}
