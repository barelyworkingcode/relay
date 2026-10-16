package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// transcriptWait bounds the wait for a Claude process to exit before its
// transcript is removed.
const transcriptWait = 15 * time.Second

var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// claudePIDForArgv is the pid of the line in `ps -axwwo pid=,command=` output
// whose executable base name is claude and whose command line holds needle; 0
// if none. The base-name test skips a shell or ssh client that only carries the
// command line.
func claudePIDForArgv(psOut, needle string) int {
	for _, line := range strings.Split(psOut, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || filepath.Base(f[1]) != "claude" || !strings.Contains(line, needle) {
			continue
		}
		if pid, err := strconv.Atoi(f[0]); err == nil && pid > 0 {
			return pid
		}
	}
	return 0
}

// claudeSessionForPID is the sessionId in Claude's registry file for pid.
func claudeSessionForPID(pid int) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("no home directory")
	}
	raw, err := os.ReadFile(filepath.Join(home, ".claude", "sessions", strconv.Itoa(pid)+".json"))
	if err != nil {
		return "", errors.New("registry file unreadable: " + errnoName(err))
	}
	var rec struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return "", errors.New("registry file unparseable")
	}
	if !validUUID(rec.SessionID) {
		return "", errors.New("registry sessionId is not a UUID")
	}
	return rec.SessionID, nil
}

// removeTranscript waits until gone answers "", then deletes each
// ~/.claude/projects/*/<id>.jsonl that holds token, here or, with onHost, on
// dropInHostTarget. Deliberate: it writes one stderr line and returns nothing,
// so the cleanup can never change a journey's result. A file is removed only
// when its name is the learned id and its content holds the run's token.
func removeTranscript(journey, id, token string, onHost bool, gone func(context.Context) string) {
	line := func(rest string) { fmt.Fprintf(os.Stderr, "transcript %s: %s\n", journey, rest) }
	if !validUUID(id) {
		line("id unknown: not a conversation id")
		return
	}
	if !tokenPattern.MatchString(token) {
		line(id + " kept: bad token")
		return
	}
	ctx := context.Background()
	if gone == nil {
		line(id + " kept: process still running")
		return
	}
	why := "process still running"
	if !pollUntil(ctx, transcriptWait, func() bool {
		why = gone(ctx)
		return why == ""
	}) {
		line(id + " kept: " + why)
		return
	}
	if onHost {
		line(id + " " + removeHostTranscript(ctx, id, token))
		return
	}
	line(id + " " + removeLocalTranscript(id, token))
}

func removeLocalTranscript(id, token string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "kept: no home directory"
	}
	matches, err := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", id+".jsonl"))
	if err != nil {
		return "kept: bad pattern"
	}
	for _, f := range matches {
		raw, err := os.ReadFile(f)
		if err != nil {
			return "kept: " + errnoName(err)
		}
		if !bytes.Contains(raw, []byte(token)) {
			return "kept: no run token"
		}
	}
	deleted := 0
	for _, f := range matches {
		if err := os.Remove(f); err != nil {
			return keptAfter(deleted, errnoName(err))
		}
		deleted++
	}
	if deleted == 0 {
		return "absent"
	}
	return fmt.Sprintf("deleted %d", deleted)
}

// keptAfter words a failed delete without hiding files already removed.
func keptAfter(deleted int, why string) string {
	if deleted == 0 {
		return "kept: " + why
	}
	return fmt.Sprintf("deleted %d, kept the rest: %s", deleted, why)
}

// removeHostTranscript runs the same guard on the loopback host. The script
// runs under sh so the login shell's handling of an unmatched glob cannot
// abort it; id and token are validated, and the script holds no single quote.
func removeHostTranscript(ctx context.Context, id, token string) string {
	script := `for f in "$HOME"/.claude/projects/*/` + id + `.jsonl; do [ -f "$f" ] || continue; ` +
		`grep -q -F -- "` + token + `" "$f" || { echo kept: no run token; exit 0; }; done; n=0; ` +
		`for f in "$HOME"/.claude/projects/*/` + id + `.jsonl; do [ -f "$f" ] || continue; ` +
		`if rm -- "$f" 2>/dev/null; then n=$((n+1)); elif [ $n -gt 0 ]; then echo deleted $n, kept the rest: rm failed; exit 0; ` +
		`else echo kept: rm failed; exit 0; fi; done; if [ $n -gt 0 ]; then echo deleted $n; else echo absent; fi`
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", dropInHostTarget, "sh -c '"+script+"'").Output()
	if err != nil {
		return "kept: ssh failed"
	}
	res := strings.TrimSpace(string(out))
	switch {
	case res == "absent", strings.HasPrefix(res, "deleted "), strings.HasPrefix(res, "kept: "):
		return res
	}
	return "kept: unexpected answer"
}

// errnoName is a short code for err, never a path.
func errnoName(err error) string {
	var en syscall.Errno
	if !errors.As(err, &en) {
		return "error"
	}
	switch en {
	case syscall.EPERM:
		return "EPERM"
	case syscall.EACCES:
		return "EACCES"
	case syscall.ENOENT:
		return "ENOENT"
	case syscall.EROFS:
		return "EROFS"
	case syscall.EBUSY:
		return "EBUSY"
	case syscall.EIO:
		return "EIO"
	}
	return "errno " + strconv.Itoa(int(en))
}

func pidGone(pid int) func(context.Context) string {
	return func(context.Context) string {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return ""
		}
		return "process still running"
	}
}
