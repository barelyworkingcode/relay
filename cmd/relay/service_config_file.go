package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tidwall/jsonc"

	"github.com/barelyworkingcode/relay/internal/bridge"
	"github.com/barelyworkingcode/relay/internal/config"
)

// maxConfigFileBytes caps both reads and writes of a service config file.
// Resource-exhaustion defense (cf. maxStatusBodyBytes in
// service_status_client.go).
const maxConfigFileBytes = 1 << 20

// resolveConfigPath re-validates a service-declared config path against live
// filesystem state: after symlink eval it must be a regular file, within
// the size cap, and its resolved real path must stay within allowedRoot
// (the service's WorkingDir when set, else the config file's own
// directory) -- catching a symlink that points outside it. The service is
// already token-authenticated and could write this file directly anyway;
// this gate is defense in depth and the single boundary everything
// downstream trusts. Returns the validated os.FileInfo so callers can
// re-verify (os.SameFile) that what they open is what was validated here,
// closing the stat->open TOCTOU window.
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
	// Both the root and the file must already exist (services seed their
	// config at startup); a missing file is an error the UI surfaces, not a
	// silent create.
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

// readConfigFile returns the file's raw bytes as opaque text -- comments and
// key order are preserved because relay never round-trips through a struct.
// want is the FileInfo resolveConfigPath validated; the opened descriptor is
// checked against it with os.SameFile so a path swapped to a different file
// between resolve and open is rejected rather than read.
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
// write. Comments survive on disk only because the caller writes the
// ORIGINAL edited bytes -- relay does not re-marshal here.
func validateConfigText(text []byte, format string) error {
	if int64(len(text)) > maxConfigFileBytes {
		return fmt.Errorf("config text exceeds %d byte cap", maxConfigFileBytes)
	}
	var probe any
	switch format {
	case bridge.ConfigFormatJSON:
		return json.Unmarshal(text, &probe)
	default:
		return json.Unmarshal(jsonc.ToJSON(text), &probe)
	}
}

// writeConfigFile atomically writes edited config text, preserving the file's
// existing mode. Caps the write as a final resource-exhaustion guard.
func writeConfigFile(realPath string, text []byte, perm os.FileMode) error {
	if int64(len(text)) > maxConfigFileBytes {
		return fmt.Errorf("refusing to write %d bytes (cap %d)", len(text), maxConfigFileBytes)
	}
	return config.AtomicWriteFile(realPath, text, perm)
}
