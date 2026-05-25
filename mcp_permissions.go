package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// TccResetTimeout caps the --check-permissions spawn so a buggy MCP can't
// hang the IPC handler indefinitely. The MCP just reads TCC status and exits
// near-instantly under normal conditions; this is a safety net.
const TccResetTimeout = 60 * time.Second

// parseTccServices parses a comma-separated list of TCC service short names
// from the --tcc-services CLI flag, trimming whitespace and dropping empties.
func parseTccServices(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// tccutilServiceName maps the short names we accept on --tcc-services to the
// canonical names tccutil(1) recognizes. Unrecognized names pass through
// unchanged so we don't silently drop a service the user wrote explicitly.
func tccutilServiceName(short string) string {
	switch strings.ToLower(short) {
	case "calendar", "calendars":
		return "Calendar"
	case "contacts", "addressbook":
		return "AddressBook"
	case "reminders":
		return "Reminders"
	case "microphone", "mic":
		return "Microphone"
	case "camera":
		return "Camera"
	case "appleevents", "automation":
		return "AppleEvents"
	case "photos":
		return "Photos"
	case "screencapture", "screenrecording":
		return "ScreenCapture"
	case "fda", "fulldisk", "fulldiskaccess":
		return "SystemPolicyAllFiles"
	case "location":
		// Location is per-system, not per-app via tccutil. Skipped.
		return ""
	default:
		return short
	}
}

// ResetMcpPermissionsResult summarizes what ResetMcpPermissions did so the UI
// can render a meaningful confirmation/error message.
type ResetMcpPermissionsResult struct {
	BundleID       string   `json:"bundle_id"`
	ResetServices  []string `json:"reset_services"`
	SkippedReasons []string `json:"skipped_reasons,omitempty"`
	SpawnOutput    string   `json:"spawn_output"`
}

// ResetMcpPermissions performs the install-time TCC dance for an MCP:
//   1. Resolve the MCP binary's bundle identifier by reading the .app's
//      Info.plist (walks up from the command path to find Contents/Info.plist).
//   2. Run `tccutil reset <Service> <bundleID>` for each declared TCC service.
//   3. Fire Relay-side TCC primers (Calendar / Contacts / Reminders), which
//      surface "Relay wants to access X" prompts from Relay's own process.
//      Grants flow to the MCP via responsible-parent attribution at runtime.
//      See mcp_permissions_darwin.go.
//   4. Spawn the MCP binary with --check-permissions using the same
//      exec.Command shape as external_mcp.go's normal stdio spawn so the
//      post-state status report comes from the runtime attribution context.
//
// Wait-bound by TccResetTimeout. The MCP must support --check-permissions
// (or the deprecated --request-permissions alias); MCPs without protected
// APIs (e.g. fsMCP) should register without --tcc-services so this isn't
// offered for them.
func ResetMcpPermissions(mcp ExternalMcp) (*ResetMcpPermissionsResult, error) {
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

	// "Primer" pass: request matching grants from Relay's own process. macOS
	// suppresses TCC prompts for background apps spawned by other background
	// apps (the macmcp-via-relay-tray chain). Relay (/Applications-resident,
	// activation-capable) can prompt successfully -- its grants then flow to
	// the spawned MCP via TCC's responsible-parent attribution at runtime.
	// Skipped if the platform doesn't expose primer functions (e.g. tests).
	primeRelayTccPermissions(mcp.TccServices, result)

	// Spawn the MCP with --check-permissions so it prints its current TCC
	// status (post-primer) into result.SpawnOutput for the UI summary.
	// Using exec.Command directly (same shape as external_mcp.go's normal
	// stdio spawn) means TCC attribution matches the runtime path. We do
	// NOT use `open` -- that would reparent to launchd and break the
	// responsible-parent chain the primer just established.
	cmd := exec.Command(mcp.Command, "--check-permissions")
	mergeEnv(cmd, mcp.Env)

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
		// normal exit (success or non-zero); both are reportable
	case <-time.After(TccResetTimeout):
		_ = cmd.Process.Kill()
		<-done
		result.SpawnOutput = buf.String() + "\n[timed out after " + TccResetTimeout.String() + "]"
		return result, nil
	}

	result.SpawnOutput = buf.String()
	return result, nil
}

// bundleIDFromCommand resolves the CFBundleIdentifier for an MCP binary.
//
// MCPs typically register a path like ~/.local/bin/macmcp — a symlink into
// macmcp.app/Contents/MacOS/macmcp. After resolving symlinks, this walks up
// looking for an Info.plist sibling of MacOS/ (i.e. Contents/Info.plist).
// Returns an error if the binary isn't inside a .app bundle.
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

// readBundleIdentifier parses CFBundleIdentifier out of an Info.plist XML file.
// Uses a minimal streaming parser to avoid pulling in a plist dependency for
// this single field.
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
