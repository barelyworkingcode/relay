package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/barelyworkingcode/relay/internal/bridge"
)

// Pidfiles let the next tray session reclaim services orphaned when the
// current process is SIGKILLed or force-quit -- see ReclaimOrphans in
// service_registry.go for the recovery flow.

func pidFileDir() (string, error) {
	dir := filepath.Join(bridge.ConfigDir(), "run")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create run directory: %w", err)
	}
	return dir, nil
}

func pidFilePath(id string) (string, error) {
	dir, err := pidFileDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".pid"), nil
}

func writePidFile(id string, pid int) error {
	path, err := pidFilePath(id)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0600)
}

func removePidFile(id string) {
	path, err := pidFilePath(id)
	if err != nil {
		return
	}
	_ = os.Remove(path)
}

// readPidFile returns 0 with a nil error when the file does not exist, so
// callers can treat "no orphan to reclaim" and "no pidfile written yet" the
// same way.
func readPidFile(id string) (int, error) {
	path, err := pidFilePath(id)
	if err != nil {
		return 0, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("parse pidfile %s: %w", path, err)
	}
	return pid, nil
}
