package hostapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// concurrentPostStatus POSTs body to path over client and returns only the
// status code (or an error) -- never calling t.Fatalf, so it is safe to run
// from a goroutine other than the one running the test function (testing.T
// explicitly forbids FailNow/Fatalf off the test's own goroutine).
func concurrentPostStatus(client *http.Client, url, bearer string, body any) (int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// TestLaunch_ConcurrentDuplicateSessionID_NoOrphan is this package's mirror
// of internal/sessions/terminal's own
// TestManager_Create_ConcurrentDuplicateID_NoOrphan (R-S6's F2 race fix):
// fire the same "pty" /launch body -- same session_id -- from N goroutines
// at once and confirm exactly one wins (201), the rest are refused (409),
// and the terminal.Manager backing this server ends up with exactly one
// live entry for that id, never two and never zero (an orphaned, untracked
// process). This package's own handleLaunch does no existence-check of its
// own before calling terminals.Create -- it relies entirely on
// terminal.Manager.Create's atomic reserve-then-spawn-then-fill (the
// pattern this test proves end to end, through the real HTTP dispatch, not
// just against Manager.Create directly as terminal's own test already
// does).
func TestLaunch_ConcurrentDuplicateSessionID_NoOrphan(t *testing.T) {
	_, target := buildBinaries(t)
	terminals, sessions := buildManagers(t)
	_, internalSock, _, bearer := startServerWithManagers(t, os.Getpid(), terminals, sessions)
	client := unixClient(internalSock)

	body := launchBody("sess-race-dup", []string{target, "-sleep", "2s", "-exit-code", "0"})

	const n = 6
	var wg sync.WaitGroup
	statuses := make([]int, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			statuses[i], errs[i] = concurrentPostStatus(client, "http://h/launch", bearer, body)
		}()
	}
	wg.Wait()

	var created, conflict int
	for i, code := range statuses {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflict++
		default:
			t.Errorf("request %d: unexpected status %d", i, code)
		}
	}
	if created != 1 {
		t.Fatalf("created = %d, want exactly 1 (statuses=%v)", created, statuses)
	}
	if conflict != n-1 {
		t.Fatalf("conflict = %d, want %d (statuses=%v)", conflict, n-1, statuses)
	}

	live := terminals.ListSummary()
	if len(live) != 1 {
		t.Fatalf("terminal.Manager has %d live entries after the race, want exactly 1 (no orphan, no double-registration): %+v", len(live), live)
	}
}

// writeEnvDumpScript writes a shell script that, on exec, dumps its own real
// environment to the file named by the RH_TEST_OUT_ENV env var, then exits.
// Mirrors internal/sessions/provider's own writeEnvArgvDumpScript
// (support_test.go) -- the established convention in this codebase for
// proving a secret-stripping claim against a *real* spawned process's *real*
// environment, not just against the env-builder function in isolation.
func writeEnvDumpScript(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "envdump.sh")
	script := "#!/bin/sh\n" +
		"env > \"$RH_TEST_OUT_ENV.tmp\"\n" +
		"mv \"$RH_TEST_OUT_ENV.tmp\" \"$RH_TEST_OUT_ENV\"\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write env dump script: %v", err)
	}
	return path
}

// TestLaunch_PTY_HostSecretNeverReachesRealChildEnv covers the forwarded
// R-S5/R-S6 finding that hostapi's own spawn path (historically
// hostapi/launch.go, since removed by the host-dispatch rewiring that made
// this package a pure dispatcher onto terminal.Manager) left cmd.Env nil,
// inheriting the host relay-sessions process's full environment -- including
// its own service secrets -- into a launched session's real child. The host
// process here (this test binary) is given a real, fake-shaped
// RELAY_SERVICE_TOKEN, exactly the credential relay-sessions' own Hello
// would hold; the assertion is against the dumped environment of the real
// spawned target process, not against childBaseEnv() in isolation, per this
// codebase's own established precedent
// (provider.TestClaudeProvider_RealSpawn_NoSecretsInRealEnvOrArgv).
func TestLaunch_PTY_HostSecretNeverReachesRealChildEnv(t *testing.T) {
	dir := mkShortTempDir(t, "hostapi-envdump-")
	script := writeEnvDumpScript(t, dir)
	envOut := filepath.Join(dir, "env.out")

	t.Setenv("RELAY_SERVICE_TOKEN", "leaked-service-token-shape")

	_, internalSock, _, bearer := startServer(t, os.Getpid())
	client := unixClient(internalSock)

	body := launchBody("sess-envleak", []string{script})
	body["env"] = map[string]string{"RH_TEST_OUT_ENV": envOut}

	resp := postJSON(t, client, "http://h/launch", bearer, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	waitForFile(t, envOut, 5)
	raw, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatalf("read env dump: %v", err)
	}
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if strings.HasPrefix(line, "RELAY_SERVICE_TOKEN=") {
			t.Fatalf("real spawned child's env leaked the host relay-sessions process's own service secret: %s", line)
		}
	}
}
