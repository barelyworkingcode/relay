#ifndef COCOA_DARWIN_H
#define COCOA_DARWIN_H

// Initialize the Cocoa application (NSApp, delegate, edit menu)
void cocoa_init_app(void);

// Enter the Cocoa run loop (blocks on main thread)
void cocoa_run_app(void);

// Create/update tray menu. Called from Go.
void cocoa_setup_tray(const unsigned char* iconRGBA, int width, int height);
void cocoa_update_menu(const char* menuJSON);  // JSON array of menu items

// Settings window
void cocoa_open_settings(const char* html);
void cocoa_settings_eval_js(const char* js);

// Clipboard
void cocoa_copy_to_clipboard(const char* text);

// Open URL in default browser
void cocoa_open_url(const char* url);

// Dispatch goDispatchCallback(ctx) on the main thread
void cocoa_dispatch_main_callback(void* ctx);

#endif
