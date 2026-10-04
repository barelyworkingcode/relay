//go:build darwin

package main

/*
#cgo LDFLAGS: -framework ApplicationServices -framework CoreFoundation
#include <ApplicationServices/ApplicationServices.h>
#include <stdlib.h>

typedef const void *axref;

static int ax_trusted(void) { return AXIsProcessTrusted() ? 1 : 0; }

static axref ax_app(int pid) {
	AXUIElementRef app = AXUIElementCreateApplication((pid_t)pid);
	if (app) AXUIElementSetMessagingTimeout(app, 2.0);
	return app;
}

static void ax_release(axref r) { if (r) CFRelease(r); }
static axref ax_retain(axref r) { return r ? CFRetain(r) : NULL; }

// Copies a string attribute as UTF-8. A non-string value reads as empty.
static int ax_string(axref el, const char *attr, char **out) {
	*out = NULL;
	CFStringRef name = CFStringCreateWithCString(NULL, attr, kCFStringEncodingUTF8);
	CFTypeRef val = NULL;
	AXError err = AXUIElementCopyAttributeValue((AXUIElementRef)el, name, &val);
	CFRelease(name);
	if (err != kAXErrorSuccess) return err;
	if (val && CFGetTypeID(val) == CFStringGetTypeID()) {
		CFIndex n = CFStringGetMaximumSizeForEncoding(CFStringGetLength(val), kCFStringEncodingUTF8) + 1;
		char *buf = malloc(n);
		if (buf && CFStringGetCString(val, buf, n, kCFStringEncodingUTF8)) *out = buf; else free(buf);
	}
	CFRelease(val);
	return kAXErrorSuccess;
}

// Copies an attribute that holds an array of elements, or one element.
static int ax_elements(axref el, const char *attr, CFArrayRef *out) {
	*out = NULL;
	CFStringRef name = CFStringCreateWithCString(NULL, attr, kCFStringEncodingUTF8);
	CFTypeRef val = NULL;
	AXError err = AXUIElementCopyAttributeValue((AXUIElementRef)el, name, &val);
	CFRelease(name);
	if (err != kAXErrorSuccess) return err;
	if (CFGetTypeID(val) == CFArrayGetTypeID()) {
		*out = val;
	} else if (CFGetTypeID(val) == AXUIElementGetTypeID()) {
		const void *items[1] = {val};
		*out = CFArrayCreate(NULL, items, 1, &kCFTypeArrayCallBacks);
		CFRelease(val);
	} else {
		CFRelease(val);
	}
	return kAXErrorSuccess;
}

static long ax_count(CFArrayRef a) { return a ? CFArrayGetCount(a) : 0; }
static axref ax_at(CFArrayRef a, long i) { return CFArrayGetValueAtIndex(a, i); }
static void ax_free_array(CFArrayRef a) { if (a) CFRelease(a); }

static int ax_press(axref el) {
	CFStringRef name = CFStringCreateWithCString(NULL, "AXPress", kCFStringEncodingUTF8);
	AXError err = AXUIElementPerformAction((AXUIElementRef)el, name);
	CFRelease(name);
	return err;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

const (
	axErrCannotComplete  = -25204
	axErrAttrUnsupported = -25205
	axErrNoValue         = -25212

	axMaxNodes = 5000
)

var errAXDriver = errors.New("accessibility driver")

// errAXCannotComplete marks a press the target app did not answer. A status
// item opens its menu and then reports this, so the caller judges by what
// appeared.
var errAXCannotComplete = errors.New("target did not answer")

func axTrusted() bool { return C.ax_trusted() == 1 }

func axErr(op string, code C.int) error {
	if int(code) == axErrCannotComplete {
		return fmt.Errorf("%w: %s: %w", errAXDriver, op, errAXCannotComplete)
	}
	return fmt.Errorf("%w: %s: AXError %d", errAXDriver, op, int(code))
}

// axSnapshot walks one element tree and keeps a retained ref for every node
// whose press is handed out.
type axSnapshot struct {
	refs  []C.axref
	count int
}

func (s *axSnapshot) release() {
	for _, r := range s.refs {
		C.ax_release(r)
	}
	s.refs = nil
}

func (s *axSnapshot) text(el C.axref, attr string) (string, error) {
	name := C.CString(attr)
	defer C.free(unsafe.Pointer(name))
	var out *C.char
	switch code := C.ax_string(el, name, &out); int(code) {
	case 0:
	case axErrAttrUnsupported, axErrNoValue:
		return "", nil
	default:
		return "", axErr("read "+attr, code)
	}
	if out == nil {
		return "", nil
	}
	defer C.free(unsafe.Pointer(out))
	return C.GoString(out), nil
}

func (s *axSnapshot) elements(el C.axref, attr string) (C.CFArrayRef, error) {
	name := C.CString(attr)
	defer C.free(unsafe.Pointer(name))
	var out C.CFArrayRef
	switch code := C.ax_elements(el, name, &out); int(code) {
	case 0:
		return out, nil
	case axErrAttrUnsupported, axErrNoValue:
		return 0, nil
	default:
		return 0, axErr("read "+attr, code)
	}
}

func (s *axSnapshot) node(el C.axref) (*axNode, error) {
	if s.count++; s.count > axMaxNodes {
		return nil, fmt.Errorf("%w: more than %d nodes in one snapshot", errAXDriver, axMaxNodes)
	}
	n := &axNode{}
	var err error
	if n.Role, err = s.text(el, "AXRole"); err != nil {
		return nil, err
	}
	if n.Subrole, err = s.text(el, "AXSubrole"); err != nil {
		return nil, err
	}
	if n.Help, err = s.text(el, "AXHelp"); err != nil {
		return nil, err
	}
	for _, attr := range []string{"AXTitle", "AXDescription", "AXValue"} {
		if n.Label, err = s.text(el, attr); err != nil || n.Label != "" {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	held := C.ax_retain(el)
	s.refs = append(s.refs, held)
	n.press = func() error {
		if code := C.ax_press(held); code != 0 {
			return axErr("AXPress", code)
		}
		return nil
	}
	kids, err := s.elements(el, "AXChildren")
	if err != nil {
		return nil, err
	}
	defer C.ax_free_array(kids)
	for i := C.long(0); i < C.ax_count(kids); i++ {
		c, err := s.node(C.ax_at(kids, i))
		if err != nil {
			return nil, err
		}
		n.Children = append(n.Children, c)
	}
	return n, nil
}

// axRoots snapshots the elements an app attribute names. An app that does
// not answer is a driver error, not an empty tree.
func axRoots(pid int, attr string) ([]*axNode, func(), error) {
	app := C.ax_app(C.int(pid))
	if app == nil {
		return nil, func() {}, fmt.Errorf("%w: no element for pid %d", errAXDriver, pid)
	}
	defer C.ax_release(app)
	s := &axSnapshot{}
	roots, err := s.elements(app, attr)
	if err != nil {
		s.release()
		return nil, func() {}, err
	}
	defer C.ax_free_array(roots)
	var out []*axNode
	for i := C.long(0); i < C.ax_count(roots); i++ {
		n, err := s.node(C.ax_at(roots, i))
		if err != nil {
			s.release()
			return nil, func() {}, err
		}
		out = append(out, n)
	}
	return out, s.release, nil
}

func axExtras(pid int) (*axNode, func(), error) {
	roots, release, err := axRoots(pid, "AXExtrasMenuBar")
	if err != nil {
		return nil, release, err
	}
	if len(roots) == 0 {
		release()
		return nil, func() {}, fmt.Errorf("%w: pid %d has no extras menu bar", errAXDriver, pid)
	}
	return roots[0], release, nil
}

func axWindows(pid int) ([]*axNode, func(), error) { return axRoots(pid, "AXWindows") }
