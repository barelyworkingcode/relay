package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"relaygo/bridge"
)

// A var rather than a const so a test can point it at a path that does not
// exist and exercise the fail-closed branch without depending on
// sandbox-exec actually being missing from the machine running the suite.
var sandboxExecPath = "/usr/bin/sandbox-exec"

// Verified working on macOS 26.4 arm64 against a Go binary and against
// ripgrep (fsMCP v3 integration, R5) — a failure here is not a clean refusal,
// it is a silent narrowing of what "sandboxed" means, so do not edit this
// without re-verifying on a real machine.
const sandboxProfileCommon = `(version 1)
(import "bsd.sb")
(allow process-exec*)
(allow process-fork)
(deny file-read* file-write*)
(allow file-read* file-map-executable
  (subpath "/usr") (subpath "/System") (subpath "/Library") (subpath "/bin")
  (subpath "/sbin") (subpath "/opt/homebrew") (subpath "/private/var/db")
  (subpath "/dev")
  (literal "/") (literal "/private") (literal "/private/tmp") (literal "/tmp"))
(allow file-write-data (subpath "/dev"))
`

const sandboxGrantReadWrite = `(allow file-read* file-write* (subpath (param "GRANT")))
`

const sandboxGrantReadOnly = `(allow file-read* (subpath (param "GRANT")))
`

func sandboxDir() string {
	return filepath.Join(bridge.ConfigDir(), "sandbox")
}

// The grant directory itself never appears in the profile file: it is passed
// as a `-D GRANT=` parameter at spawn time (prepareStdioLaunch) instead, so
// the profile's own bytes are identical for every sandboxed MCP and a
// directory name containing a quote or a backslash cannot touch its syntax.
// Writing it under relay's own config dir, rather than a temp file, means an
// attacker with write access to a shared temp directory cannot swap the
// profile out from under sandbox-exec between relay writing it and the child
// reading it.
func ensureSandboxProfile(readOnly bool) (string, error) {
	name, body := "sandbox-rw.sb", sandboxProfileCommon+sandboxGrantReadWrite
	if readOnly {
		name, body = "sandbox-ro.sb", sandboxProfileCommon+sandboxGrantReadOnly
	}
	dir := sandboxDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("sandbox profile dir: %w", err)
	}
	path := filepath.Join(dir, name)
	if err := atomicWriteFile(path, []byte(body), 0600); err != nil {
		return "", fmt.Errorf("write sandbox profile: %w", err)
	}
	return path, nil
}

// BOTH "-name" and "--name" are accepted, because Go's flag package accepts
// both and an MCP spelled "-root /srv/notes" starts and serves exactly as
// "--root /srv/notes" does. Matching only the double-dash form meant relay
// found no root, took prepareStdioLaunch's pass-through branch, and spawned
// the MCP with no seatbelt and no audited root — the one direction this must
// never fail in, reached by a spelling nothing rejects.
//
// ok is false for an argument that is not a flag, including the bare "--"
// that ends flag parsing.
func splitFlag(arg string) (name, value string, hasValue, ok bool) {
	if len(arg) < 2 || arg[0] != '-' {
		return "", "", false, false
	}
	trimmed := strings.TrimPrefix(arg[1:], "-")
	if trimmed == "" {
		return "", "", false, false
	}
	if n, v, found := strings.Cut(trimmed, "="); found {
		return n, v, true, true
	}
	return trimmed, "", false, true
}

// This is the one narrow, declared thing relay parses out of an MCP's own argv
// (fsMCP v3 integration, R2/R5) — not a general argument parser, and it must
// not grow into one: relay does not know what any OTHER MCP's flags mean.
// --root is special only because relay put it there when the MCP was
// registered and needs it back for two declared reasons, the audit record and
// the seatbelt grant.
func stdioRootFlag(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			return "", false
		}
		name, value, hasValue, ok := splitFlag(args[i])
		if !ok || name != "root" {
			continue
		}
		if hasValue {
			return value, true
		}
		if i+1 < len(args) {
			return args[i+1], true
		}
		return "", false
	}
	return "", false
}

// This decides which of the two seatbelt profiles applies. It does not, and
// must not, decide relay's own access mode (ADR-011 decision 2) — this reads
// fsMCP's process-level flag back only to pick a profile.
func stdioReadOnlyFlag(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		name, value, hasValue, ok := splitFlag(a)
		if !ok || name != "read-only" {
			continue
		}
		if !hasValue {
			return true
		}
		// Go's flag package parses a boolean flag's value with ParseBool, so
		// "1", "t" and "TRUE" all mean "true"; reading only the literal
		// "true" left an fsMCP started with "--read-only=1" running under
		// the read-WRITE profile.
		v, err := strconv.ParseBool(value)
		return err == nil && v
	}
	return false
}

// Without a --root argument, the command and args pass through unchanged —
// this mechanism is deliberately narrow (R5) and does not sandbox an MCP
// relay was never told the boundary of.
//
// With one, the child is spawned under seatbelt instead of directly, and this
// function fails closed on everything that could go wrong: sandbox-exec
// missing, the root failing to resolve through symlinks, or the profile
// failing to write. There is no fallback branch that runs the MCP
// unsandboxed — every non-nil error here means the caller must not spawn
// anything.
//
// cfg.ResolvedRoot is set on success so the caller, and through it the audit
// log, can name the directory without re-deriving it from Args.
func prepareStdioLaunch(cfg *ExternalMcp) (command string, args []string, err error) {
	root, ok := stdioRootFlag(cfg.Args)
	if !ok {
		// Said out loud, at the same level as the seatbelt log line below:
		// "no seatbelt" and "the log line scrolled past" must not look
		// identical to an operator reading back why a directory was reachable.
		slog.Info("spawning MCP unsandboxed: no --root argument to bound it with", "id", cfg.ID)
		return cfg.Command, cfg.Args, nil
	}
	// Seatbelt matches real paths; a grant spelled through a symlink would
	// leave the sandbox and the intended directory disagreeing about what is
	// inside it.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", nil, fmt.Errorf("resolve --root %q: %w", root, err)
	}
	if _, statErr := os.Stat(sandboxExecPath); statErr != nil {
		return "", nil, fmt.Errorf("sandbox-exec unavailable, refusing to start %q unsandboxed: %w", cfg.ID, statErr)
	}
	readOnly := stdioReadOnlyFlag(cfg.Args)
	profile, err := ensureSandboxProfile(readOnly)
	if err != nil {
		return "", nil, fmt.Errorf("sandbox profile for %q: %w", cfg.ID, err)
	}
	cfg.ResolvedRoot = resolved

	wrapped := make([]string, 0, len(cfg.Args)+5)
	wrapped = append(wrapped, "-f", profile, "-D", "GRANT="+resolved, cfg.Command)
	wrapped = append(wrapped, cfg.Args...)

	mode := "read-write"
	if readOnly {
		mode = "read-only"
	}
	slog.Info("spawning MCP under seatbelt", "id", cfg.ID, "root", resolved, "mode", mode)
	return sandboxExecPath, wrapped, nil
}
