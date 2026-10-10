//go:build relaytest

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/barelyworkingcode/relay/internal/logging"
	"github.com/barelyworkingcode/relay/internal/sshhost"
)

const (
	sshStubFileName = "test-ssh.json"
	maxSSHStubBytes = 64 << 10
)

type sshStubFile struct {
	Command string `json:"command"`
}

// installSSHStub points every ssh_argv at the stub named in X/test-ssh.json.
// An absent file leaves real ssh. An invalid file is an error, so a test aimed
// at a stub never falls back to a real host.
func installSSHStub(configDir string) error {
	command, ok, err := readSSHStub(configDir)
	if err != nil || !ok {
		return err
	}
	sshhost.SetTestCommand(command)
	slog.Warn("test build: ssh is replaced by a stub program", "config_dir", configDir, "command", command)
	logging.BeginEvent(context.Background(), "debug.ssh.stub").
		Set("command", command).End(logging.OutcomeOK, "", nil)
	return nil
}

// readSSHStub returns the stub path. ok is false when the seams do not act on
// configDir or the file is absent. The file gets the private-file rules the
// presence outcome file gets.
func readSSHStub(configDir string) (command string, ok bool, err error) {
	if !seamsActive(configDir) {
		return "", false, nil
	}
	path := filepath.Join(configDir, sshStubFileName)
	invalid := func(fault string) error { return fmt.Errorf("%s: %s", path, fault) }
	// O_NOFOLLOW: the file must itself be the regular file checked below.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, invalid("cannot be opened as a regular file")
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", false, invalid("cannot be inspected")
	}
	st, statOK := info.Sys().(*syscall.Stat_t)
	switch {
	case !info.Mode().IsRegular():
		return "", false, invalid("is not a regular file")
	case !statOK || int(st.Uid) != os.Getuid():
		return "", false, invalid("is not owned by the current user")
	case info.Mode().Perm()&0o077 != 0:
		return "", false, invalid("has group or other permission bits")
	case info.Size() > maxSSHStubBytes:
		return "", false, invalid("is larger than 64 KiB")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxSSHStubBytes+1))
	if err != nil || len(raw) > maxSSHStubBytes {
		return "", false, invalid("cannot be read")
	}
	var cfg sshStubFile
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return "", false, invalid("is not the expected JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", false, invalid("has data after the JSON object")
	}
	if !filepath.IsAbs(cfg.Command) {
		return "", false, invalid("command is not an absolute path")
	}
	cmd, err := os.Stat(cfg.Command)
	switch {
	case err != nil:
		return "", false, invalid("command cannot be inspected")
	case !cmd.Mode().IsRegular():
		return "", false, invalid("command is not a regular file")
	case cmd.Mode().Perm()&0o111 == 0:
		return "", false, invalid("command is not executable")
	}
	return cfg.Command, true, nil
}
