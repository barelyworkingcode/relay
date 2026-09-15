//go:build darwin

package membership

/*
#include <libproc.h>
#include <sys/proc_info.h>
*/
import "C"

import "unsafe"

// DarwinSource is the real Source: one proc_pidinfo(PROC_PIDTBSDINFO) call
// per pid, no caching. Its zero value is ready to use.
type DarwinSource struct{}

// NewSource returns the darwin ancestry reader.
func NewSource() Source { return DarwinSource{} }

// Info reads pid's ppid and start time. A pid that no longer exists, or one
// this process lacks permission to query (PID 1 for a non-root caller), both
// report ok=false: the caller's job is to fail closed either way, not to
// tell them apart.
func (DarwinSource) Info(pid int) (ProcInfo, bool) {
	if pid <= 0 {
		return ProcInfo{}, false
	}
	var info C.struct_proc_bsdinfo
	size := C.int(unsafe.Sizeof(info))
	n := C.proc_pidinfo(C.int(pid), C.PROC_PIDTBSDINFO, 0, unsafe.Pointer(&info), size)
	if n < size {
		return ProcInfo{}, false
	}
	return ProcInfo{
		PID:       pid,
		PPID:      int(info.pbi_ppid),
		StartSec:  int64(info.pbi_start_tvsec),
		StartUsec: int32(info.pbi_start_tvusec),
	}, true
}
