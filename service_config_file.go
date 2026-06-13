package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tidwall/jsonc"

	"relaygo/bridge"
)

// maxConfigFileBytes caps both reads and writes of a service config file.
// Resource-exhaustion defense (cf. maxStatusBodyBytes in
// service_status_client.go). Service config files are kilobytes; 1 MiB is
// generous headroom.
const maxConfigFileBytes = 1 << 20

// resolveConfigPath re-validates a service-declared config path at USE time and
// returns the resolved real path. The manifest's schema-level Validate
// (bridge/manifest.go) already checked absolute + no "..". Here we additionally
// enforce, against live filesystem state:
//   - the path resolves (after symlink eval) to a regular file
//   - that resolved real path stays within allowedRoot (no symlink escape)
//   - the file is within the size cap
//
// allowedRoot is the service's configured WorkingDir when set (an explicit,
// possibly broader root). When empty it defaults to the directory the service
// declared its config file in — the tightest sensible bound, so the editor
// works without every service having to register a --workdir. In both cases the
// resolved file must stay within the root, which catches a symlink pointing out
// of it. The service is service-token authenticated at RegisterManifest and
// could write its own config file directly anyway; this gate is defense in
// depth (no symlink escape, regular file, bounded size), and is the single
// boundary everything downstream trusts.
//
// Returns the validated os.FileInfo alongside the resolved path so callers can
// re-verify (via os.SameFile) that the descriptor they open is the very file
// validated here — closing the stat→open TOCTOU window against a path swap.
func resolveConfigPath(decl *bridge.ConfigDecl, allowedRoot string) (string, os.FileInfo, error) {
	if decl == nil {
		return "", nil, fmt.Errorf("service declares no config file")
	}
	if !filepath.IsAbs(decl.Path) {
		return "", nil, fmt.Errorf("config path %q is not absolute", decl.Path)
	}
	if allowedRoot == "" {
		allowedRoot = filepath.Dir(decl.Path)
	}
	// EvalSymlinks on both sides so a symlink pointing outside the root is
	// caught by the containment check rather than smuggling us out of it. Both
	// the root and the file must already exist (services seed their config at
	// startup); a missing file is an error the UI surfaces, not a silent create.
	rootReal, err := filepath.EvalSymlinks(allowedRoot)
	if err != nil {
		return "", nil, fmt.Errorf("resolve config root %q: %w", allowedRoot, err)
	}
	real, err := filepath.EvalSymlinks(decl.Path)
	if err != nil {
		return "", nil, fmt.Errorf("resolve config path %q: %w", decl.Path, err)
	}
	rel, err := filepath.Rel(rootReal, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", nil, fmt.Errorf("config path %q escapes allowed root %q", decl.Path, rootReal)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", nil, fmt.Errorf("stat config path: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("config path %q is not a regular file", decl.Path)
	}
	if info.Size() > maxConfigFileBytes {
		return "", nil, fmt.Errorf("config file %q exceeds %d byte cap", decl.Path, maxConfigFileBytes)
	}
	return real, info, nil
}

// readConfigFile returns the file's raw bytes as opaque text — comments and key
// order are preserved because relay never round-trips through a struct. Capped
// at maxConfigFileBytes (re-checked here to close the stat→read TOCTOU window).
//
// want is the FileInfo resolveConfigPath validated; the opened descriptor is
// checked against it with os.SameFile so a path swapped to a different file
// (e.g. a symlink) between resolve and open is rejected rather than read.
func readConfigFile(realPath string, want os.FileInfo) ([]byte, error) {
	f, err := os.Open(realPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if got, serr := f.Stat(); serr != nil {
		return nil, fmt.Errorf("stat opened config file: %w", serr)
	} else if want != nil && !os.SameFile(want, got) {
		return nil, fmt.Errorf("config file at %q changed between validation and open", realPath)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}
	if int64(len(data)) > maxConfigFileBytes {
		return nil, fmt.Errorf("config file exceeds %d byte cap", maxConfigFileBytes)
	}
	return data, nil
}

// validateConfigText checks that edited text parses per format before any
// write. JSONC tolerates // and /* */ comments (jsonc.ToJSON strips them before
// JSON validation). Comments survive the round-trip on disk only when the
// caller writes the ORIGINAL edited bytes — relay does not re-marshal here.
func validateConfigText(text []byte, format string) error {
	if int64(len(text)) > maxConfigFileBytes {
		return fmt.Errorf("config text exceeds %d byte cap", maxConfigFileBytes)
	}
	var probe any
	switch format {
	case bridge.ConfigFormatJSON:
		return json.Unmarshal(text, &probe)
	default: // jsonc (default)
		return json.Unmarshal(jsonc.ToJSON(text), &probe)
	}
}

// writeConfigFile atomically writes edited config text, preserving the file's
// existing mode. Caps the write as a final resource-exhaustion guard.
func writeConfigFile(realPath string, text []byte, perm os.FileMode) error {
	if int64(len(text)) > maxConfigFileBytes {
		return fmt.Errorf("refusing to write %d bytes (cap %d)", len(text), maxConfigFileBytes)
	}
	return atomicWriteFile(realPath, text, perm)
}
