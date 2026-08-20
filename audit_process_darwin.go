//go:build darwin

package main

/*
#include <libproc.h>
#include <sys/proc_info.h>
*/
import "C"

import "unsafe"

// ProcessNames returns the command name for pid and for its parent, or empty
// strings when either can't be read (the process exited, or it belongs to
// another user).
//
// Uses libproc's proc_pidinfo rather than shelling out to `ps`: this sits on
// the tool-call path, and two syscalls cost microseconds where a fork+exec
// costs milliseconds.
//
// The parent matters more than the process itself. `relay mcp` opens a fresh
// connection — and often a fresh process — per call, so the peer pid alone
// names a throwaway subprocess; its parent is the agent that actually asked for
// the tool.
func ProcessNames(pid int) (proc, parent string) {
	name, ppid, ok := procInfo(pid)
	if !ok {
		return "", ""
	}
	if ppid > 0 {
		if parentName, _, ok := procInfo(ppid); ok {
			parent = parentName
		}
	}
	return name, parent
}

// procInfo reads one process's name and parent pid via PROC_PIDTBSDINFO.
func procInfo(pid int) (name string, ppid int, ok bool) {
	if pid <= 0 {
		return "", 0, false
	}
	var info C.struct_proc_bsdinfo
	size := C.int(unsafe.Sizeof(info))
	n := C.proc_pidinfo(C.int(pid), C.PROC_PIDTBSDINFO, 0, unsafe.Pointer(&info), size)
	if n < size {
		// Short read means the pid is gone or unreadable; report "unknown"
		// rather than a partially-populated struct.
		return "", 0, false
	}
	// pbi_name is the fuller name (32 chars) but is empty for some processes,
	// where pbi_comm (16 chars) still has the short form.
	name = C.GoString(&info.pbi_name[0])
	if name == "" {
		name = C.GoString(&info.pbi_comm[0])
	}
	return name, int(info.pbi_ppid), true
}
