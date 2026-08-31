package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa -framework WebKit -framework EventKit -framework Contacts -framework UserNotifications
#include "cocoa_darwin.h"
#include <stdlib.h>
*/
import "C"
import (
	"sync"
	"unsafe"
)

// A closure cannot cross the cgo boundary, so callbacks are parked here and
// C carries only the uintptr key.
var (
	cbMu   sync.Mutex
	cbMap  = make(map[uintptr]func())
	cbNext uintptr
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
	defer C.free(unsafe.Pointer(cs))
	C.cocoa_update_menu(cs)
}

func cocoaOpenSettings(html string) {
	cs := C.CString(html)
	defer C.free(unsafe.Pointer(cs))
	C.cocoa_open_settings(cs)
}

func cocoaSettingsEvalJS(js string) {
	cs := C.CString(js)
	defer C.free(unsafe.Pointer(cs))
	C.cocoa_settings_eval_js(cs)
}

func dispatchToMain(fn func()) {
	id := storeCallback(fn)
	C.cocoa_dispatch_main_callback(C.uintptr_t(id))
}

// macOS suppresses TCC prompts for .accessory (LSUIElement) apps, so every
// batch of cocoaRequestTcc* calls must be bracketed by these two.
func cocoaBeginForegroundActivation() { C.cocoa_begin_foreground_activation() }
func cocoaEndForegroundActivation()   { C.cocoa_end_foreground_activation() }

// Each blocks the calling goroutine — never the main thread — until the user
// answers or timeoutSec elapses.
func cocoaRequestTccCalendar(timeoutSec int) bool {
	return C.cocoa_request_tcc_calendar(C.int(timeoutSec)) != 0
}

func cocoaRequestTccContacts(timeoutSec int) bool {
	return C.cocoa_request_tcc_contacts(C.int(timeoutSec)) != 0
}

func cocoaRequestTccReminders(timeoutSec int) bool {
	return C.cocoa_request_tcc_reminders(C.int(timeoutSec)) != 0
}

type DarwinPlatform struct{}

func NewPlatform() Platform { return &DarwinPlatform{} }

func (p *DarwinPlatform) Init()                           { cocoaInitApp() }
func (p *DarwinPlatform) Run()                            { cocoaRunApp() }
func (p *DarwinPlatform) SetupTray(rgba []byte, w, h int) { cocoaSetupTray(rgba, w, h) }
func (p *DarwinPlatform) UpdateMenu(menuJSON string)      { cocoaUpdateMenu(menuJSON) }
func (p *DarwinPlatform) OpenSettings(html string)        { cocoaOpenSettings(html) }
func (p *DarwinPlatform) EvalSettingsJS(js string)        { cocoaSettingsEvalJS(js) }
func (p *DarwinPlatform) DispatchToMain(fn func())        { dispatchToMain(fn) }

func (p *DarwinPlatform) OpenURL(url string) {
	cs := C.CString(url)
	defer C.free(unsafe.Pointer(cs))
	C.cocoa_open_url(cs)
}

func (p *DarwinPlatform) Notify(title, body string) {
	ct := C.CString(title)
	defer C.free(unsafe.Pointer(ct))
	cb := C.CString(body)
	defer C.free(unsafe.Pointer(cb))
	C.cocoa_notify(ct, cb)
}

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

//export goOnNotificationClick
func goOnNotificationClick() {
	if appInstance != nil {
		appInstance.onNotificationClick()
	}
}

// goOnNotificationsDenied is the only route Objective-C has into relay's
// slog output. It carries no appInstance check on purpose: a denial is worth
// recording whether or not the tray finished coming up.
//
//export goOnNotificationsDenied
func goOnNotificationsDenied(detail *C.char) {
	reportNotificationsDenied(C.GoString(detail))
}

//export goOnAppTerminate
func goOnAppTerminate() {
	if appInstance != nil {
		appInstance.cleanup()
	}
}

//export goDispatchCallback
func goDispatchCallback(ctx C.uintptr_t) {
	id := uintptr(ctx)
	fn := loadCallback(id)
	if fn != nil {
		fn()
	}
}
