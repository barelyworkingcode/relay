#import "dialog_darwin.h"
#import <ApplicationServices/ApplicationServices.h>
#import <Foundation/Foundation.h>
#import <IOKit/IOKitLib.h>
#import <Security/Security.h>
#include <libproc.h>
#include <math.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

static const char *kAgentExecutable = "coreautha";
static CFStringRef kAgentRequirement =
	CFSTR("anchor apple and identifier \"com.apple.LocalAuthentication.UIAgent\"");

enum { kKeyReturn = 36, kKeyEscape = 53 };

static int is_agent_process(pid_t pid, NSString *ownerName) {
	char path[PROC_PIDPATHINFO_MAXSIZE];
	if (proc_pidpath(pid, path, sizeof path) > 0) {
		const char *base = strrchr(path, '/');
		base = base ? base + 1 : path;
		if (strcmp(base, kAgentExecutable) == 0) {
			return 1;
		}
	}
	return [ownerName isKindOfClass:[NSString class]] &&
		[ownerName isEqualToString:@(kAgentExecutable)];
}

int dbp_list_windows(dbp_window *out, int max) {
	@autoreleasepool {
		CFArrayRef list = CGWindowListCopyWindowInfo(
			kCGWindowListOptionOnScreenOnly | kCGWindowListExcludeDesktopElements, kCGNullWindowID);
		if (list == NULL) {
			return -1;
		}
		NSArray *windows = CFBridgingRelease(list);
		int n = 0;
		for (NSDictionary *w in windows) {
			NSNumber *pid = w[(id)kCGWindowOwnerPID];
			NSNumber *num = w[(id)kCGWindowNumber];
			if (pid == nil || num == nil || !is_agent_process(pid.intValue, w[(id)kCGWindowOwnerName])) {
				continue;
			}
			if (n < max) {
				out[n].window = num.unsignedIntValue;
				out[n].pid = pid.intValue;
			}
			n++;
		}
		return n;
	}
}

int dbp_owner_ok(int32_t pid, char *detail, int detail_len) {
	@autoreleasepool {
		SecCodeRef code = NULL;
		NSDictionary *attrs = @{(id)kSecGuestAttributePid : @(pid)};
		OSStatus st = SecCodeCopyGuestWithAttributes(NULL, (CFDictionaryRef)attrs, kSecCSDefaultFlags, &code);
		if (st != errSecSuccess || code == NULL) {
			snprintf(detail, detail_len, "cannot read the code signature of the dialog's owner (pid %d, OSStatus %d)", pid, (int)st);
			return 0;
		}
		SecRequirementRef req = NULL;
		st = SecRequirementCreateWithString(kAgentRequirement, kSecCSDefaultFlags, &req);
		if (st == errSecSuccess) {
			st = SecCodeCheckValidity(code, kSecCSDefaultFlags, req);
			CFRelease(req);
		}
		CFRelease(code);
		if (st != errSecSuccess) {
			snprintf(detail, detail_len, "the dialog's owner (pid %d) is not the Apple-signed LocalAuthentication agent (OSStatus %d)", pid, (int)st);
			return 0;
		}
		return 1;
	}
}

static CFTypeRef ax_copy(AXUIElementRef el, CFStringRef attr) {
	CFTypeRef v = NULL;
	if (AXUIElementCopyAttributeValue(el, attr, &v) != kAXErrorSuccess) {
		return NULL;
	}
	return v;
}

static int ax_string_equals(AXUIElementRef el, CFStringRef attr, CFStringRef want) {
	CFTypeRef v = ax_copy(el, attr);
	int eq = v != NULL && CFGetTypeID(v) == CFStringGetTypeID() && CFEqual(v, want);
	if (v != NULL) {
		CFRelease(v);
	}
	return eq;
}

static int ax_frame(AXUIElementRef el, CGRect *r) {
	CFTypeRef p = ax_copy(el, kAXPositionAttribute);
	CFTypeRef s = ax_copy(el, kAXSizeAttribute);
	int ok = p != NULL && s != NULL &&
		AXValueGetValue((AXValueRef)p, kAXValueCGPointType, &r->origin) &&
		AXValueGetValue((AXValueRef)s, kAXValueCGSizeType, &r->size);
	if (p != NULL) {
		CFRelease(p);
	}
	if (s != NULL) {
		CFRelease(s);
	}
	return ok;
}

static int cg_bounds(uint32_t window, CGRect *r) {
	const void *ids[1] = {(const void *)(uintptr_t)window};
	CFArrayRef arr = CFArrayCreate(NULL, ids, 1, NULL);
	CFArrayRef desc = CGWindowListCreateDescriptionFromArray(arr);
	CFRelease(arr);
	int ok = 0;
	if (desc != NULL && CFArrayGetCount(desc) == 1) {
		CFDictionaryRef d = CFArrayGetValueAtIndex(desc, 0);
		CFDictionaryRef b = CFDictionaryGetValue(d, kCGWindowBounds);
		ok = b != NULL && CGRectMakeWithDictionaryRepresentation(b, r);
	}
	if (desc != NULL) {
		CFRelease(desc);
	}
	return ok;
}

static int frames_match(CGRect a, CGRect b) {
	return fabs(a.origin.x - b.origin.x) <= 2 && fabs(a.origin.y - b.origin.y) <= 2 &&
		fabs(a.size.width - b.size.width) <= 2 && fabs(a.size.height - b.size.height) <= 2;
}

static AXUIElementRef ax_match_in(AXUIElementRef app, CFStringRef attr, CGRect want) {
	CFTypeRef list = ax_copy(app, attr);
	AXUIElementRef found = NULL;
	if (list != NULL && CFGetTypeID(list) == CFArrayGetTypeID()) {
		for (CFIndex i = 0; i < CFArrayGetCount(list) && found == NULL; i++) {
			AXUIElementRef w = (AXUIElementRef)CFArrayGetValueAtIndex(list, i);
			CGRect r;
			if (ax_frame(w, &r) && frames_match(r, want)) {
				found = (AXUIElementRef)CFRetain(w);
			}
		}
	}
	if (list != NULL) {
		CFRelease(list);
	}
	return found;
}

// The AX window is matched to the CG window by frame because the public API
// has no mapping between the two window identities.
static AXUIElementRef ax_window_for(AXUIElementRef app, uint32_t window) {
	CGRect want;
	if (!cg_bounds(window, &want)) {
		return NULL;
	}
	AXUIElementRef w = ax_match_in(app, kAXWindowsAttribute, want);
	return w != NULL ? w : ax_match_in(app, kAXChildrenAttribute, want);
}

static AXUIElementRef ax_app(int32_t pid) {
	AXUIElementRef app = AXUIElementCreateApplication(pid);
	AXUIElementSetMessagingTimeout(app, 1.0f);
	return app;
}

static void ax_collect_text(AXUIElementRef el, NSMutableString *out, int depth, int *budget) {
	if (depth > 12 || *budget <= 0) {
		return;
	}
	(*budget)--;
	// Deliberate: the password field is never read, whatever it holds.
	if (ax_string_equals(el, kAXSubroleAttribute, kAXSecureTextFieldSubrole)) {
		return;
	}
	CFStringRef attrs[] = {kAXTitleAttribute, kAXValueAttribute, kAXDescriptionAttribute};
	for (size_t i = 0; i < sizeof attrs / sizeof attrs[0]; i++) {
		CFTypeRef v = ax_copy(el, attrs[i]);
		if (v != NULL && CFGetTypeID(v) == CFStringGetTypeID() && CFStringGetLength(v) > 0) {
			[out appendString:(__bridge NSString *)v];
			[out appendString:@"\n"];
		}
		if (v != NULL) {
			CFRelease(v);
		}
	}
	CFTypeRef kids = ax_copy(el, kAXChildrenAttribute);
	if (kids != NULL && CFGetTypeID(kids) == CFArrayGetTypeID()) {
		for (CFIndex i = 0; i < CFArrayGetCount(kids); i++) {
			ax_collect_text((AXUIElementRef)CFArrayGetValueAtIndex(kids, i), out, depth + 1, budget);
		}
	}
	if (kids != NULL) {
		CFRelease(kids);
	}
}

int dbp_window_text(int32_t pid, uint32_t window, char *buf, int buf_len) {
	@autoreleasepool {
		AXUIElementRef app = ax_app(pid);
		AXUIElementRef win = ax_window_for(app, window);
		CFRelease(app);
		if (win == NULL) {
			return -1;
		}
		NSMutableString *text = [NSMutableString string];
		int budget = 400;
		ax_collect_text(win, text, 0, &budget);
		CFRelease(win);
		if (![text getCString:buf maxLength:buf_len encoding:NSUTF8StringEncoding]) {
			NSData *d = [text dataUsingEncoding:NSUTF8StringEncoding allowLossyConversion:YES];
			size_t n = MIN((size_t)buf_len - 1, d.length);
			memcpy(buf, d.bytes, n);
			buf[n] = 0;
		}
		return (int)strlen(buf);
	}
}

static AXUIElementRef ax_find(AXUIElementRef el, CFStringRef attr, CFStringRef want, int depth) {
	if (depth > 12) {
		return NULL;
	}
	if (ax_string_equals(el, attr, want)) {
		return (AXUIElementRef)CFRetain(el);
	}
	AXUIElementRef found = NULL;
	CFTypeRef kids = ax_copy(el, kAXChildrenAttribute);
	if (kids != NULL && CFGetTypeID(kids) == CFArrayGetTypeID()) {
		for (CFIndex i = 0; i < CFArrayGetCount(kids) && found == NULL; i++) {
			found = ax_find((AXUIElementRef)CFArrayGetValueAtIndex(kids, i), attr, want, depth + 1);
		}
	}
	if (kids != NULL) {
		CFRelease(kids);
	}
	return found;
}

static int focused_app_is(pid_t pid) {
	AXUIElementRef sys = AXUIElementCreateSystemWide();
	CFTypeRef app = ax_copy(sys, kAXFocusedApplicationAttribute);
	CFRelease(sys);
	pid_t got = -1;
	if (app != NULL) {
		AXUIElementGetPid((AXUIElementRef)app, &got);
		CFRelease(app);
	}
	return got == pid;
}

static int field_has_focus(AXUIElementRef app, AXUIElementRef field) {
	CFTypeRef f = ax_copy(app, kAXFocusedUIElementAttribute);
	int ok = f != NULL && CFEqual(f, field);
	if (f != NULL) {
		CFRelease(f);
	}
	return ok;
}

static void bring_forward(AXUIElementRef app, AXUIElementRef win) {
	AXUIElementSetAttributeValue(app, kAXFrontmostAttribute, kCFBooleanTrue);
	if (win != NULL) {
		AXUIElementPerformAction(win, kAXRaiseAction);
	}
}

static void post_keyboard(CGKeyCode key, const UniChar *chars, int n) {
	for (int down = 1; down >= 0; down--) {
		CGEventRef ev = CGEventCreateKeyboardEvent(NULL, key, down);
		CGEventSetFlags(ev, 0);
		if (chars != NULL) {
			CGEventKeyboardSetUnicodeString(ev, n, chars);
		}
		CGEventPost(kCGHIDEventTap, ev);
		CFRelease(ev);
		usleep(chars != NULL ? 2000 : 10000);
	}
}

static void post_text(const uint16_t *pw, int n) {
	for (int i = 0; i < n;) {
		int m = MIN(20, n - i);
		// Subtle: a chunk must not end between the halves of a surrogate pair.
		if (m < n - i && pw[i + m - 1] >= 0xD800 && pw[i + m - 1] <= 0xDBFF) {
			m--;
		}
		post_keyboard(0, (const UniChar *)(pw + i), m);
		i += m;
		usleep(12000);
	}
}

int dbp_answer(int32_t pid, uint32_t window, const uint16_t *pw, int n, char *detail, int detail_len) {
	@autoreleasepool {
		AXUIElementRef app = ax_app(pid);
		AXUIElementRef win = ax_window_for(app, window);
		AXUIElementRef field = win != NULL ? ax_find(win, kAXSubroleAttribute, kAXSecureTextFieldSubrole, 0) : NULL;
		int ok = 0;
		if (win == NULL) {
			snprintf(detail, detail_len, "cannot reach the dialog through Accessibility");
		} else if (field == NULL) {
			snprintf(detail, detail_len, "the dialog has no password field");
		} else {
			bring_forward(app, win);
			AXUIElementSetAttributeValue(field, kAXFocusedAttribute, kCFBooleanTrue);
			usleep(150000);
			// Deliberate: keystrokes go to whatever holds focus, so nothing is
			// typed unless the agent is frontmost and its password field focused.
			if (!focused_app_is(pid)) {
				snprintf(detail, detail_len, "the dialog's agent did not become the focused application");
			} else if (!field_has_focus(app, field)) {
				snprintf(detail, detail_len, "the password field did not take focus");
			} else {
				post_text(pw, n);
				usleep(50000);
				post_keyboard(kKeyReturn, NULL, 0);
				snprintf(detail, detail_len, "typed the password and pressed Return");
				ok = 1;
			}
		}
		if (field != NULL) {
			CFRelease(field);
		}
		if (win != NULL) {
			CFRelease(win);
		}
		CFRelease(app);
		return ok;
	}
}

static int press(AXUIElementRef button) {
	return button != NULL && AXUIElementPerformAction(button, kAXPressAction) == kAXErrorSuccess;
}

int dbp_cancel(int32_t pid, uint32_t window, char *detail, int detail_len) {
	@autoreleasepool {
		AXUIElementRef app = ax_app(pid);
		AXUIElementRef win = ax_window_for(app, window);
		int ok = 0;
		if (win != NULL) {
			CFTypeRef button = ax_copy(win, kAXCancelButtonAttribute);
			if (button != NULL && CFGetTypeID(button) == AXUIElementGetTypeID() && press((AXUIElementRef)button)) {
				snprintf(detail, detail_len, "pressed the dialog's cancel button");
				ok = 1;
			}
			if (button != NULL) {
				CFRelease(button);
			}
			if (!ok) {
				AXUIElementRef titled = ax_find(win, kAXTitleAttribute, CFSTR("Cancel"), 0);
				if (press(titled)) {
					snprintf(detail, detail_len, "pressed Cancel");
					ok = 1;
				}
				if (titled != NULL) {
					CFRelease(titled);
				}
			}
		}
		if (!ok) {
			bring_forward(app, win);
			usleep(150000);
			if (focused_app_is(pid)) {
				post_keyboard(kKeyEscape, NULL, 0);
				snprintf(detail, detail_len, "sent Escape");
				ok = 1;
			} else {
				snprintf(detail, detail_len, "no cancel button through Accessibility, and the agent did not take focus for Escape");
			}
		}
		if (win != NULL) {
			CFRelease(win);
		}
		CFRelease(app);
		return ok;
	}
}

static int dict_bool(CFDictionaryRef d, CFStringRef key) {
	CFTypeRef v = CFDictionaryGetValue(d, key);
	return v != NULL && CFGetTypeID(v) == CFBooleanGetTypeID() && CFBooleanGetValue(v);
}

void dbp_probe_session(dbp_session *st) {
	memset(st, 0, sizeof *st);
	st->ax_trusted = AXIsProcessTrusted() ? 1 : 0;
	st->post_events = CGPreflightPostEventAccess() ? 1 : 0;
	CFArrayRef list = CGWindowListCopyWindowInfo(kCGWindowListOptionOnScreenOnly, kCGNullWindowID);
	st->window_list = list != NULL;
	if (list != NULL) {
		CFRelease(list);
	}

	// IOConsoleUsers answers from any process, where the CGSession dictionary
	// is empty outside a GUI session.
	io_registry_entry_t root = IORegistryGetRootEntry(kIOMainPortDefault);
	CFTypeRef users = IORegistryEntryCreateCFProperty(root, CFSTR("IOConsoleUsers"), NULL, 0);
	IOObjectRelease(root);
	if (users == NULL) {
		return;
	}
	if (CFGetTypeID(users) == CFArrayGetTypeID()) {
		for (CFIndex i = 0; i < CFArrayGetCount(users); i++) {
			CFDictionaryRef u = CFArrayGetValueAtIndex(users, i);
			if (CFGetTypeID(u) != CFDictionaryGetTypeID()) {
				continue;
			}
			CFNumberRef uidRef = CFDictionaryGetValue(u, CFSTR("kCGSSessionUserIDKey"));
			long long uid = -1;
			if (uidRef == NULL || !CFNumberGetValue(uidRef, kCFNumberLongLongType, &uid) || uid != (long long)getuid()) {
				continue;
			}
			st->on_console = dict_bool(u, CFSTR("kCGSSessionOnConsoleKey"));
			st->locked = dict_bool(u, CFSTR("CGSSessionScreenIsLocked"));
		}
	}
	CFRelease(users);
}
