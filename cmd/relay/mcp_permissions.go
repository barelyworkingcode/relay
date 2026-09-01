package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"github.com/barelyworkingcode/relay/internal/config"
	"github.com/barelyworkingcode/relay/internal/service"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Caps the --check-permissions spawn so a buggy MCP can't hang the IPC
// handler indefinitely; the MCP reads TCC status and exits near-instantly
// under normal conditions.
const TccResetTimeout = 60 * time.Second

// Unknown names pass through unchanged so a user-supplied service we don't
// recognize still reaches tccutil verbatim instead of being silently dropped.
func canonicalTccService(name string) string {
	switch strings.ToLower(name) {
	case "calendar", "calendars":
		return "calendar"
	case "contacts", "addressbook":
		return "contacts"
	case "reminders":
		return "reminders"
	case "microphone", "mic":
		return "microphone"
	case "camera":
		return "camera"
	case "appleevents", "automation":
		return "appleevents"
	case "photos":
		return "photos"
	case "screencapture", "screenrecording":
		return "screencapture"
	case "fda", "fulldisk", "fulldiskaccess":
		return "fulldiskaccess"
	case "location":
		return "location"
	default:
		return strings.ToLower(name)
	}
}

func parseTccServices(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, canonicalTccService(s))
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Returns "" for canonical names that have no tccutil mapping (e.g.
// "location" -- per-system, not per-app).
func tccutilServiceName(canonical string) string {
	switch canonical {
	case "calendar":
		return "Calendar"
	case "contacts":
		return "AddressBook"
	case "reminders":
		return "Reminders"
	case "microphone":
		return "Microphone"
	case "camera":
		return "Camera"
	case "appleevents":
		return "AppleEvents"
	case "photos":
		return "Photos"
	case "screencapture":
		return "ScreenCapture"
	case "fulldiskaccess":
		return "SystemPolicyAllFiles"
	case "location":
		return ""
	default:
		return canonical
	}
}

type ResetMcpPermissionsResult struct {
	BundleID       string   `json:"bundle_id"`
	ResetServices  []string `json:"reset_services"`
	SkippedReasons []string `json:"skipped_reasons,omitempty"`
	SpawnOutput    string   `json:"spawn_output"`
}

// The MCP must support --check-permissions; MCPs without protected APIs
// (e.g. fsMCP) should register without --tcc-services so this isn't offered
// for them.
func ResetMcpPermissions(mcp config.ExternalMcp) (*ResetMcpPermissionsResult, error) {
	if len(mcp.TccServices) == 0 {
		return nil, fmt.Errorf("MCP %q declares no TCC services (--tcc-services not set at registration)", mcp.ID)
	}
	if mcp.Command == "" {
		return nil, fmt.Errorf("MCP %q has no command (HTTP MCPs do not have TCC permissions)", mcp.ID)
	}

	bundleID, err := bundleIDFromCommand(mcp.Command)
	if err != nil {
		return nil, fmt.Errorf("could not resolve bundle ID for %s: %w", mcp.Command, err)
	}

	result := &ResetMcpPermissionsResult{BundleID: bundleID}

	for _, svc := range mcp.TccServices {
		canonical := tccutilServiceName(svc)
		if canonical == "" {
			result.SkippedReasons = append(result.SkippedReasons,
				fmt.Sprintf("%s: no tccutil mapping (skipped)", svc))
			continue
		}
		out, err := exec.Command("tccutil", "reset", canonical, bundleID).CombinedOutput()
		if err != nil {
			result.SkippedReasons = append(result.SkippedReasons,
				fmt.Sprintf("%s (%s): %v — %s", svc, canonical, err, strings.TrimSpace(string(out))))
			continue
		}
		result.ResetServices = append(result.ResetServices, canonical)
	}

	// macOS suppresses TCC prompts for background apps spawned by other
	// background apps (the macmcp-via-relay-tray chain); Relay itself is
	// /Applications-resident and activation-capable, so it can prompt on the
	// MCP's behalf and the grant flows to the MCP via responsible-parent
	// attribution at runtime.
	primeRelayTccPermissions(mcp.TccServices, result)

	// exec.Command directly, not `open`: `open` would reparent the child to
	// launchd and break the responsible-parent chain the primer just
	// established.
	cmd := exec.Command(mcp.Command, "--check-permissions")
	env, err := revealEnvOrErr(mcp.Env)
	if err != nil {
		return nil, fmt.Errorf("env: %w", err)
	}
	service.MergeEnv(cmd, env)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s --check-permissions: %w", mcp.Command, err)
	}
	go func() { done <- cmd.Wait() }()

	select {
	case <-done:
	case <-time.After(TccResetTimeout):
		_ = cmd.Process.Kill()
		<-done
		result.SpawnOutput = buf.String() + "\n[timed out after " + TccResetTimeout.String() + "]"
		return result, nil
	}

	result.SpawnOutput = buf.String()
	return result, nil
}

func bundleIDFromCommand(command string) (string, error) {
	resolved, err := filepath.EvalSymlinks(command)
	if err != nil {
		return "", fmt.Errorf("eval symlinks: %w", err)
	}
	// Standard layout: <bundle>.app/Contents/MacOS/<exe>. Walk up at most
	// 3 levels looking for the Info.plist.
	dir := filepath.Dir(resolved)
	for i := 0; i < 3; i++ {
		plistPath := filepath.Join(dir, "Info.plist")
		if info, statErr := os.Stat(plistPath); statErr == nil && !info.IsDir() {
			return readBundleIdentifier(plistPath)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("no Info.plist found near %s (binary not in a .app bundle?)", resolved)
}

// Minimal streaming parser to avoid pulling in a plist dependency for this
// single field.
func readBundleIdentifier(plistPath string) (string, error) {
	data, err := os.ReadFile(plistPath)
	if err != nil {
		return "", err
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	var (
		inKey       bool
		keyText     string
		expectValue bool
	)
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "key":
				inKey = true
				keyText = ""
			case "string":
				if expectValue {
					var s string
					if err := dec.DecodeElement(&s, &t); err == nil {
						return s, nil
					}
				}
			}
		case xml.CharData:
			if inKey {
				keyText += string(t)
			}
		case xml.EndElement:
			if t.Name.Local == "key" {
				inKey = false
				if strings.TrimSpace(keyText) == "CFBundleIdentifier" {
					expectValue = true
				}
			}
		}
	}
	return "", fmt.Errorf("CFBundleIdentifier not found in %s", plistPath)
}
