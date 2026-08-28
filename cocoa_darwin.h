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
