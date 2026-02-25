package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa -framework WebKit
#include "cocoa_darwin.h"
#include <stdlib.h>
*/
import "C"
import (
	"sync"
	"sync/atomic"
	"unsafe"
)

// ---------------------------------------------------------------------------
// Callback registry for dispatchToMain
// ---------------------------------------------------------------------------

var (
	cbMu    sync.Mutex
	cbMap   = make(map[uintptr]func())
	cbNext  uintptr
)

func storeCallback(fn func()) uintptr {
	cbMu.Lock()
	defer cbMu.Unlock()
	cbNext++
	id := cbNext
	cbMap[id] = fn
	return id
}

func loadCallback(id uintptr) func() {
	cbMu.Lock()
	defer cbMu.Unlock()
	fn := cbMap[id]
	delete(cbMap, id)
	return fn
}

// ---------------------------------------------------------------------------
// Go wrappers for C functions
// ---------------------------------------------------------------------------

func cocoaInitApp() {
	C.cocoa_init_app()
}

func cocoaRunApp() {
	C.cocoa_run_app()
}

func cocoaSetupTray(rgba []byte, w, h int) {
	C.cocoa_setup_tray((*C.uchar)(&rgba[0]), C.int(w), C.int(h))
}

func cocoaUpdateMenu(menuJSON string) {
	cs := C.CString(menuJSON)
	C.cocoa_update_menu(cs)
	C.free(unsafe.Pointer(cs))
}

func cocoaOpenSettings(html string) {
	cs := C.CString(html)
	C.cocoa_open_settings(cs)
	C.free(unsafe.Pointer(cs))
}

func cocoaSettingsEvalJS(js string) {
	cs := C.CString(js)
	C.cocoa_settings_eval_js(cs)
	C.free(unsafe.Pointer(cs))
}

func cocoaCopyToClipboard(text string) {
	cs := C.CString(text)
	C.cocoa_copy_to_clipboard(cs)
	C.free(unsafe.Pointer(cs))
}

// dispatchToMain schedules a Go function to run on the main (UI) thread.
func dispatchToMain(fn func()) {
	id := storeCallback(fn)
	C.cocoa_dispatch_main_callback(unsafe.Pointer(id))
}

// ---------------------------------------------------------------------------
// DarwinPlatform implements Platform using Cocoa via cgo.
// ---------------------------------------------------------------------------

type DarwinPlatform struct{}

func NewPlatform() Platform { return &DarwinPlatform{} }

func (p *DarwinPlatform) Init()                            { cocoaInitApp() }
func (p *DarwinPlatform) Run()                             { cocoaRunApp() }
func (p *DarwinPlatform) SetupTray(rgba []byte, w, h int)  { cocoaSetupTray(rgba, w, h) }
func (p *DarwinPlatform) UpdateMenu(menuJSON string)       { cocoaUpdateMenu(menuJSON) }
func (p *DarwinPlatform) OpenSettings(html string)         { cocoaOpenSettings(html) }
func (p *DarwinPlatform) EvalSettingsJS(js string)         { cocoaSettingsEvalJS(js) }
func (p *DarwinPlatform) CopyToClipboard(text string)      { cocoaCopyToClipboard(text) }
func (p *DarwinPlatform) DispatchToMain(fn func())         { dispatchToMain(fn) }

func (p *DarwinPlatform) OpenURL(url string) {
	cs := C.CString(url)
	defer C.free(unsafe.Pointer(cs))
	C.cocoa_open_url(cs)
}

// ---------------------------------------------------------------------------
// appRunning tracks whether the Cocoa app is still alive (for cleanup)
// ---------------------------------------------------------------------------

var appRunning atomic.Bool

// ---------------------------------------------------------------------------
// Exported callbacks invoked from Objective-C
// ---------------------------------------------------------------------------

//export goOnMenuClick
func goOnMenuClick(itemID C.int) {
	if appInstance != nil {
		appInstance.onMenuClick(int(itemID))
	}
}

//export goOnSettingsIpc
func goOnSettingsIpc(msg *C.char) {
	if appInstance != nil {
		appInstance.onSettingsIpc(C.GoString(msg))
	}
}

//export goOnSettingsClose
func goOnSettingsClose() {
	if appInstance != nil {
		appInstance.onSettingsClose()
	}
}

//export goOnAppTerminate
func goOnAppTerminate() {
	if appInstance != nil {
		appInstance.cleanup()
	}
}

//export goDispatchCallback
func goDispatchCallback(ctx unsafe.Pointer) {
	id := uintptr(ctx)
	fn := loadCallback(id)
	if fn != nil {
		fn()
	}
}
