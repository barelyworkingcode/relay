#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#include "cocoa_darwin.h"
#include <stdlib.h>

// Go callback declarations (implemented in cocoa_darwin.go with //export)
extern void goOnMenuClick(int itemID);
extern void goOnSettingsIpc(const char* msg);
extern void goOnSettingsClose(void);

// ---------------------------------------------------------------------------
// IPC handler for WKWebView -> Go
// ---------------------------------------------------------------------------
@interface IpcHandler : NSObject <WKScriptMessageHandler>
@end

@implementation IpcHandler
- (void)userContentController:(WKUserContentController *)uc
      didReceiveScriptMessage:(WKScriptMessage *)message {
    NSString *body = (NSString *)message.body;
    goOnSettingsIpc([body UTF8String]);
}
@end

// ---------------------------------------------------------------------------
// Settings window delegate
// ---------------------------------------------------------------------------
@interface SettingsWindowController : NSObject <NSWindowDelegate>
@property (strong) NSWindow *window;
@property (strong) WKWebView *webView;
@property (strong) IpcHandler *ipcHandler;
@end

static SettingsWindowController *settingsCtrl = nil;

@implementation SettingsWindowController
- (BOOL)windowShouldClose:(NSWindow *)sender {
    goOnSettingsClose();
    return YES;
}

- (void)windowWillClose:(NSNotification *)notification {
    [self.webView.configuration.userContentController removeScriptMessageHandlerForName:@"ipc"];
    self.webView = nil;
    self.window = nil;
    settingsCtrl = nil;
}
@end

// ---------------------------------------------------------------------------
// App delegate
// ---------------------------------------------------------------------------
@interface AppDelegate : NSObject <NSApplicationDelegate>
@property (strong) NSStatusItem *statusItem;
@end

@implementation AppDelegate
- (void)applicationDidFinishLaunching:(NSNotification *)notification {
    // No dock icon, no activation -- handled by LSUIElement in Info.plist
}
@end

// ---------------------------------------------------------------------------
// Statics
// ---------------------------------------------------------------------------
static AppDelegate *appDelegate = nil;

// ---------------------------------------------------------------------------
// Menu action target
// ---------------------------------------------------------------------------
@interface MenuTarget : NSObject
- (void)menuItemClicked:(NSMenuItem *)sender;
@end

static MenuTarget *menuTarget = nil;

@implementation MenuTarget
- (void)menuItemClicked:(NSMenuItem *)sender {
    int itemID = (int)sender.tag;
    goOnMenuClick(itemID);
}
@end

// ---------------------------------------------------------------------------
// cocoa_init_app  -- create NSApplication, delegate, menu target, edit menu
// ---------------------------------------------------------------------------
void cocoa_init_app(void) {
    [NSApplication sharedApplication];
    [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];

    appDelegate = [[AppDelegate alloc] init];
    [NSApp setDelegate:appDelegate];

    menuTarget = [[MenuTarget alloc] init];

    // Set up Edit menu so Cmd+C/V/X/A work in WKWebView
    NSMenu *mainMenu = [[NSMenu alloc] init];
    NSMenuItem *editMenuItem = [[NSMenuItem alloc] initWithTitle:@"Edit" action:nil keyEquivalent:@""];
    NSMenu *editMenu = [[NSMenu alloc] initWithTitle:@"Edit"];
    [editMenu addItemWithTitle:@"Undo" action:@selector(undo:) keyEquivalent:@"z"];
    [editMenu addItemWithTitle:@"Redo" action:@selector(redo:) keyEquivalent:@"Z"];
    [editMenu addItem:[NSMenuItem separatorItem]];
    [editMenu addItemWithTitle:@"Cut" action:@selector(cut:) keyEquivalent:@"x"];
    [editMenu addItemWithTitle:@"Copy" action:@selector(copy:) keyEquivalent:@"c"];
    [editMenu addItemWithTitle:@"Paste" action:@selector(paste:) keyEquivalent:@"v"];
    [editMenu addItemWithTitle:@"Select All" action:@selector(selectAll:) keyEquivalent:@"a"];
    [editMenuItem setSubmenu:editMenu];
    [mainMenu addItem:editMenuItem];
    [NSApp setMainMenu:mainMenu];
}

// ---------------------------------------------------------------------------
// cocoa_run_app  -- enter the Cocoa run loop (blocks)
// ---------------------------------------------------------------------------
void cocoa_run_app(void) {
    [NSApp run];
}

// ---------------------------------------------------------------------------
// cocoa_setup_tray
// ---------------------------------------------------------------------------
void cocoa_setup_tray(const unsigned char* iconRGBA, int width, int height) {
    NSStatusBar *bar = [NSStatusBar systemStatusBar];
    appDelegate.statusItem = [bar statusItemWithLength:NSVariableStatusItemLength];

    // Convert RGBA data to NSImage
    NSBitmapImageRep *rep = [[NSBitmapImageRep alloc]
        initWithBitmapDataPlanes:NULL
                      pixelsWide:width
                      pixelsHigh:height
                   bitsPerSample:8
                 samplesPerPixel:4
                        hasAlpha:YES
                        isPlanar:NO
                  colorSpaceName:NSCalibratedRGBColorSpace
                    bitmapFormat:NSBitmapFormatAlphaNonpremultiplied
                     bytesPerRow:width * 4
                    bitsPerPixel:32];

    memcpy([rep bitmapData], iconRGBA, width * height * 4);

    NSImage *image = [[NSImage alloc] initWithSize:NSMakeSize(width, height)];
    [image addRepresentation:rep];
    [image setTemplate:YES];  // Adapts to dark/light mode

    appDelegate.statusItem.button.image = image;
    appDelegate.statusItem.button.toolTip = @"Relay";

    // Create empty menu (will be populated by cocoa_update_menu)
    NSMenu *menu = [[NSMenu alloc] init];
    appDelegate.statusItem.menu = menu;
}

// ---------------------------------------------------------------------------
// cocoa_update_menu
// ---------------------------------------------------------------------------
void cocoa_update_menu(const char* menuJSON) {
    NSString *jsonStr = [NSString stringWithUTF8String:menuJSON];
    NSData *data = [jsonStr dataUsingEncoding:NSUTF8StringEncoding];
    NSError *error = nil;
    NSArray *items = [NSJSONSerialization JSONObjectWithData:data options:0 error:&error];
    if (error || !items) return;

    NSMenu *menu = appDelegate.statusItem.menu;
    [menu removeAllItems];

    for (NSDictionary *item in items) {
        NSString *title = item[@"title"];
        NSNumber *itemID = item[@"id"];
        NSNumber *enabled = item[@"enabled"];

        if ([title isEqualToString:@"-"]) {
            [menu addItem:[NSMenuItem separatorItem]];
            continue;
        }

        NSMenuItem *mi = [[NSMenuItem alloc] initWithTitle:title
                                                    action:@selector(menuItemClicked:)
                                             keyEquivalent:@""];
        mi.target = menuTarget;
        mi.tag = [itemID integerValue];
        mi.enabled = enabled ? [enabled boolValue] : YES;
        [menu addItem:mi];
    }
}

// ---------------------------------------------------------------------------
// cocoa_open_settings
// ---------------------------------------------------------------------------
void cocoa_open_settings(const char* html) {
    if (settingsCtrl && settingsCtrl.window) {
        [settingsCtrl.window makeKeyAndOrderFront:nil];
        [NSApp activateIgnoringOtherApps:YES];
        return;
    }

    NSRect frame = NSMakeRect(0, 0, 900, 650);
    NSWindow *window = [[NSWindow alloc]
        initWithContentRect:frame
                  styleMask:(NSWindowStyleMaskTitled |
                             NSWindowStyleMaskClosable |
                             NSWindowStyleMaskMiniaturizable |
                             NSWindowStyleMaskResizable)
                    backing:NSBackingStoreBuffered
                      defer:NO];
    [window setTitle:@"Relay Settings"];
    [window center];

    settingsCtrl = [[SettingsWindowController alloc] init];
    settingsCtrl.window = window;
    window.delegate = settingsCtrl;

    // WKWebView with IPC handler
    WKWebViewConfiguration *config = [[WKWebViewConfiguration alloc] init];
    WKUserContentController *uc = [[WKUserContentController alloc] init];
    settingsCtrl.ipcHandler = [[IpcHandler alloc] init];
    [uc addScriptMessageHandler:settingsCtrl.ipcHandler name:@"ipc"];
    config.userContentController = uc;

    WKWebView *webView = [[WKWebView alloc] initWithFrame:window.contentView.bounds configuration:config];
    webView.autoresizingMask = NSViewWidthSizable | NSViewHeightSizable;
    settingsCtrl.webView = webView;

    [window.contentView addSubview:webView];

    NSString *htmlStr = [NSString stringWithUTF8String:html];
    [webView loadHTMLString:htmlStr baseURL:nil];

    [window makeKeyAndOrderFront:nil];
    [NSApp activateIgnoringOtherApps:YES];
}

// ---------------------------------------------------------------------------
// cocoa_settings_eval_js
// ---------------------------------------------------------------------------
void cocoa_settings_eval_js(const char* js) {
    if (!settingsCtrl || !settingsCtrl.webView) return;
    NSString *script = [NSString stringWithUTF8String:js];
    [settingsCtrl.webView evaluateJavaScript:script completionHandler:nil];
}

// ---------------------------------------------------------------------------
// cocoa_copy_to_clipboard
// ---------------------------------------------------------------------------
void cocoa_copy_to_clipboard(const char* text) {
    NSPasteboard *pb = [NSPasteboard generalPasteboard];
    [pb clearContents];
    [pb setString:[NSString stringWithUTF8String:text] forType:NSPasteboardTypeString];
}

// ---------------------------------------------------------------------------
// cocoa_open_url
// ---------------------------------------------------------------------------
void cocoa_open_url(const char* url) {
    NSString *urlStr = [NSString stringWithUTF8String:url];
    [[NSWorkspace sharedWorkspace] openURL:[NSURL URLWithString:urlStr]];
}

// ---------------------------------------------------------------------------
// cocoa_dispatch_main_callback
// ---------------------------------------------------------------------------
extern void goDispatchCallback(void* ctx);

void cocoa_dispatch_main_callback(void* ctx) {
    dispatch_async(dispatch_get_main_queue(), ^{
        goDispatchCallback(ctx);
    });
}

