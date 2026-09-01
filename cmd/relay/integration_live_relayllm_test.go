//go:build live

package main

import (
	"context"
	"github.com/barelyworkingcode/relay/internal/config"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLive_RelayLLM_RegistersAndServesStatus(t *testing.T) {
	binPath := findRelayLLMBinary(t)
	if binPath == "" {
		t.Skip("../relayLLM/relayLLM not built; build it and re-run with -tags=live")
	}

	enhanced := NewEnhancedServiceRegistry(nil)
	router, reg := startSandboxBridge(t, enhanced)
	_ = router

	cfg := &config.ServiceConfig{
		ID:          "relayLLM",
		DisplayName: "Relay LLM",
		Command:     binPath,
	}
	if err := reg.Start(cfg); err != nil {
		t.Fatalf("Start relayLLM: %v", err)
	}
	t.Cleanup(func() { reg.Stop(cfg.ID) })

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if enhanced.Get(cfg.ID) != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	rec := enhanced.Get(cfg.ID)
	if rec == nil {
		t.Fatal("relayLLM never registered its manifest with relay")
	}
	t.Logf("relayLLM registered with %d routes", len(rec.Manifest.Routes))

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", rec.InternalSocket)
			},
		},
		Timeout: 5 * time.Second,
	}
	req, _ := http.NewRequest("GET", "http://internal/api/status", nil)
	if rec.InternalToken != "" {
		req.Header.Set("Authorization", "Bearer "+rec.InternalToken)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("relayLLM /api/status returned %d", resp.StatusCode)
	}
}

func TestLive_RelayLLM_ManifestMatchesFixture(t *testing.T) {
	binPath := findRelayLLMBinary(t)
	if binPath == "" {
		t.Skip("../relayLLM/relayLLM not built")
	}

	enhanced := NewEnhancedServiceRegistry(nil)
	_, reg := startSandboxBridge(t, enhanced)

	cfg := &config.ServiceConfig{
		ID:          "relayLLM",
		DisplayName: "Relay LLM",
		Command:     binPath,
	}
	if err := reg.Start(cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { reg.Stop(cfg.ID) })

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if enhanced.Get(cfg.ID) != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	rec := enhanced.Get(cfg.ID)
	if rec == nil {
		t.Fatal("relayLLM never registered")
	}

	expected := loadManifestFixture(t, "relayllm.json")
	expectedSet := map[string]bool{}
	for _, r := range expected.Routes {
		expectedSet[r] = true
	}
	actualSet := map[string]bool{}
	for _, r := range rec.Manifest.Routes {
		actualSet[r] = true
	}

	var missing, extra []string
	for r := range expectedSet {
		if !actualSet[r] {
			missing = append(missing, r)
		}
	}
	for r := range actualSet {
		if !expectedSet[r] {
			extra = append(extra, r)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("relayLLM manifest drift detected:\n  expected by fixture, missing from binary: %v\n  registered by binary, missing from fixture: %v\nfix: update test/fixtures/manifests/relayllm.json to match relayLLM's current manifest", missing, extra)
	}
}

func findRelayLLMBinary(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"../relayLLM/relayLLM",
		"../relayLLM/relayllm",
		"../relayLLM/bin/relayLLM",
	}
	for _, c := range candidates {
		abs, _ := filepath.Abs(c)
		if fi, err := os.Stat(abs); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return abs
		}
	}
	return ""
}
