package projectfs

/*
#cgo LDFLAGS: -framework CoreServices -framework CoreFoundation
#include <CoreServices/CoreServices.h>
#include <dispatch/dispatch.h>
#include <stdint.h>
#include <stdlib.h>

extern void relayFSEventsCallback(uintptr_t handle, size_t n, char **paths, uint32_t *flags);

static void relayFSEventsTrampoline(ConstFSEventStreamRef s, void *info, size_t n, void *paths,
                                    const FSEventStreamEventFlags flags[], const FSEventStreamEventId ids[]) {
	relayFSEventsCallback((uintptr_t)info, n, (char **)paths, (uint32_t *)flags);
}

typedef struct {
	FSEventStreamRef stream;
	dispatch_queue_t queue;
} relayStream;

// Returns NULL when the stream could not be created or started. The stream is
// live, with everything it has seen flushed, when this returns.
static relayStream *relayStartStream(const char *path, uintptr_t handle) {
	CFStringRef cfPath = CFStringCreateWithCString(NULL, path, kCFStringEncodingUTF8);
	if (!cfPath) return NULL;
	CFArrayRef paths = CFArrayCreate(NULL, (const void **)&cfPath, 1, &kCFTypeArrayCallBacks);
	CFRelease(cfPath);
	FSEventStreamContext ctx = {0, (void *)handle, NULL, NULL, NULL};
	FSEventStreamRef stream = FSEventStreamCreate(NULL, relayFSEventsTrampoline, &ctx, paths,
		kFSEventStreamEventIdSinceNow, 0.0,
		kFSEventStreamCreateFlagFileEvents | kFSEventStreamCreateFlagNoDefer);
	CFRelease(paths);
	if (!stream) return NULL;
	dispatch_queue_t queue = dispatch_queue_create("relay.projectfs.watch", DISPATCH_QUEUE_SERIAL);
	FSEventStreamSetDispatchQueue(stream, queue);
	if (!FSEventStreamStart(stream)) {
		FSEventStreamInvalidate(stream);
		FSEventStreamRelease(stream);
		dispatch_release(queue);
		return NULL;
	}
	FSEventStreamFlushSync(stream);
	relayStream *rs = malloc(sizeof(relayStream));
	rs->stream = stream;
	rs->queue = queue;
	return rs;
}

static void relayStopStream(relayStream *rs) {
	FSEventStreamStop(rs->stream);
	FSEventStreamInvalidate(rs->stream);
	FSEventStreamRelease(rs->stream);
	dispatch_release(rs->queue);
	free(rs);
}

static const char *relayPathAt(char **paths, size_t i) { return paths[i]; }
static uint32_t relayFlagAt(uint32_t *flags, size_t i) { return flags[i]; }
*/
import "C"

import (
	"context"
	"path/filepath"
	"runtime/cgo"
	"strings"
	"sync"
	"unsafe"
)

const (
	flagMustScan = C.kFSEventStreamEventFlagMustScanSubDirs | C.kFSEventStreamEventFlagUserDropped | C.kFSEventStreamEventFlagKernelDropped
	flagRename   = C.kFSEventStreamEventFlagItemCreated | C.kFSEventStreamEventFlagItemRemoved | C.kFSEventStreamEventFlagItemRenamed
	flagChange   = C.kFSEventStreamEventFlagItemModified | C.kFSEventStreamEventFlagItemInodeMetaMod |
		C.kFSEventStreamEventFlagItemXattrMod | C.kFSEventStreamEventFlagItemFinderInfoMod | C.kFSEventStreamEventFlagItemChangeOwner
)

// fsWatch is one live FSEvents stream.
type fsWatch struct {
	mu      sync.RWMutex
	stopped bool
	root    string // real path; FSEvents reports real paths
	sink    func(Event)
}

// eventKind maps FSEvents flags to the wire kind; ok is false for flags that
// carry no change (a bare directory marker).
func eventKind(flags uint32) (kind string, ok bool) {
	switch {
	case flags&flagRename != 0:
		return KindRename, true
	case flags&(flagChange|flagMustScan) != 0:
		return KindChange, true
	}
	return "", false
}

//export relayFSEventsCallback
func relayFSEventsCallback(h C.uintptr_t, n C.size_t, paths **C.char, flags *C.uint32_t) {
	w := cgo.Handle(h).Value().(*fsWatch)
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.stopped {
		return
	}
	for i := C.size_t(0); i < n; i++ {
		kind, ok := eventKind(uint32(C.relayFlagAt(flags, i)))
		if !ok {
			continue
		}
		abs := C.GoString(C.relayPathAt(paths, i))
		// The root's own events and a sibling sharing its prefix are not
		// project content.
		rest, ok := strings.CutPrefix(abs, w.root)
		if !ok || !strings.HasPrefix(rest, "/") {
			continue
		}
		w.sink(Event{Path: rest[1:], Kind: kind})
	}
}

// Watch starts an FSEvents stream on the project root. It returns once the
// stream is started and flushed, so any change after it returns is delivered.
// The sink runs on FSEvents' own queue and must not block.
func (l *local) Watch(ctx context.Context, sink func(Event)) (func(), error) {
	real, err := filepath.EvalSymlinks(l.root)
	if err != nil {
		return nil, Errf(CodeENOENT, "project root not found")
	}
	w := &fsWatch{root: strings.TrimRight(real, "/"), sink: sink}
	h := cgo.NewHandle(w)
	croot := C.CString(real)
	defer C.free(unsafe.Pointer(croot))
	rs := C.relayStartStream(croot, C.uintptr_t(h))
	if rs == nil {
		h.Delete()
		return nil, Errf(CodeUnsupported, "could not start a file watcher")
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			// The write lock waits out a callback in flight; after it no
			// callback delivers.
			w.mu.Lock()
			w.stopped = true
			w.mu.Unlock()
			C.relayStopStream(rs)
			h.Delete()
		})
	}
	go func() {
		<-ctx.Done()
		stop()
	}()
	return stop, nil
}
