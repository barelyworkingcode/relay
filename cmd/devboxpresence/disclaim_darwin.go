//go:build darwin

package main

/*
#include <errno.h>
#include <spawn.h>
#include <stdlib.h>
#include <sys/types.h>

// Private to libSystem, but stable: it makes the spawned child its own
// responsible process, so TCC judges it by its own grant.
extern int responsibility_spawnattrs_setdisclaim(posix_spawnattr_t *attr, int disclaim);

static int dbp_spawn_disclaimed(const char *path, char *const argv[], char *const envp[], pid_t *pid) {
	posix_spawnattr_t attr;
	int rc = posix_spawnattr_init(&attr);
	if (rc != 0) {
		return rc;
	}
	rc = responsibility_spawnattrs_setdisclaim(&attr, 1);
	if (rc == 0) {
		rc = posix_spawn(pid, path, NULL, &attr, argv, envp);
	}
	posix_spawnattr_destroy(&attr);
	return rc;
}
*/
import "C"

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"unsafe"
)

// runDisclaimed re-runs this binary as a child that is its own responsible
// process and relays its exit. Accessibility trust follows the responsible
// process, which under a launchd job is the job's leader, not this binary.
// ok is false when this process is already that child.
func runDisclaimed() (code int, ok bool) {
	if os.Getenv(disclaimedEnv) == "1" {
		return 0, false
	}
	exe, err := os.Executable()
	if err != nil {
		return spawnRefused(fmt.Errorf("cannot find this executable: %w", err)), true
	}
	argv := cStrings(os.Args)
	defer freeCStrings(argv)
	envp := cStrings(disclaimedEnviron(os.Environ()))
	defer freeCStrings(envp)
	cPath := C.CString(exe)
	defer C.free(unsafe.Pointer(cPath))

	// Deliberate: signals are captured before the spawn, so one that arrives
	// in between is still forwarded rather than killing only this wrapper.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)

	var pid C.pid_t
	if rc := C.dbp_spawn_disclaimed(cPath, &argv[0], &envp[0], &pid); rc != 0 {
		return spawnRefused(fmt.Errorf("posix_spawn: %w", syscall.Errno(rc))), true
	}
	child := int(pid)
	go func() {
		for s := range sigs {
			_ = syscall.Kill(child, s.(syscall.Signal))
		}
	}()
	var ws syscall.WaitStatus
	for {
		_, err := syscall.Wait4(child, &ws, 0, nil)
		if err == nil {
			break
		}
		if err != syscall.EINTR {
			return spawnRefused(fmt.Errorf("waiting for the disclaimed child: %w", err)), true
		}
	}
	if ws.Signaled() {
		return 128 + int(ws.Signal()), true
	}
	return ws.ExitStatus(), true
}

func spawnRefused(err error) int {
	res := refused("cannot re-spawn with responsibility disclaimed: %v", err)
	fmt.Fprintf(os.Stdout, "DIALOG\t%s\t%s\n", res.Outcome, res.Detail)
	return res.Code
}

// cStrings returns a NULL-terminated array of C strings.
func cStrings(ss []string) []*C.char {
	out := make([]*C.char, len(ss)+1)
	for i, s := range ss {
		out[i] = C.CString(s)
	}
	return out
}

func freeCStrings(cs []*C.char) {
	for _, c := range cs {
		if c != nil {
			C.free(unsafe.Pointer(c))
		}
	}
}
