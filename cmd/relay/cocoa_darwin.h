#ifndef COCOA_DARWIN_H
#define COCOA_DARWIN_H

#include <stdint.h>

void cocoa_init_app(void);
void cocoa_run_app(void);

void cocoa_setup_tray(const unsigned char* iconRGBA, int width, int height);
void cocoa_update_menu(const char* menuJSON);

void cocoa_open_settings(const char* html);
void cocoa_settings_eval_js(const char* js);

void cocoa_open_url(const char* url);

// Best-effort user notification. Silently does nothing when the process has
// no bundle identifier (UNUserNotificationCenter throws there). A denial by
// macOS is not silent: it reaches relay's log once, via
// goOnNotificationsDenied. Deliveries share one identifier, so a second
// banner replaces the first rather than stacking.
void cocoa_notify(const char* title, const char* body);

void cocoa_dispatch_main_callback(uintptr_t ctx);

// Each request blocks the calling thread, not the main one, and returns 1 if
// access is granted. A batch must be bracketed by begin/end_foreground_activation:
// macOS suppresses TCC prompts for .accessory apps.
void cocoa_begin_foreground_activation(void);
void cocoa_end_foreground_activation(void);
int cocoa_request_tcc_calendar(int timeoutSec);
int cocoa_request_tcc_contacts(int timeoutSec);
int cocoa_request_tcc_reminders(int timeoutSec);

#endif
