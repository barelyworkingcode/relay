#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#import <EventKit/EventKit.h>
#import <Contacts/Contacts.h>
#include "cocoa_darwin.h"
#include <stdlib.h>

// Go callback declarations (implemented in cocoa_darwin.go with //export)
extern void goOnMenuClick(int itemID);
extern void goOnSettingsIpc(const char* msg);
extern void goOnSettingsClose(void);
extern void goOnAppTerminate(void);

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
@interface SettingsWindowController : NSObject <NSWindowDelegate, WKUIDelegate>
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

// WKWebView suppresses window.alert / confirm / prompt by default; the host
// must implement these WKUIDelegate methods or the calls silently no-op,
// which means existing Settings-UI confirm() guards (delete project, rotate
// token, reset MCP permissions) all run as if the user clicked "Cancel".
// We wire them to plain NSAlert so they behave as the JS expects.

- (void)webView:(WKWebView *)webView
    runJavaScriptAlertPanelWithMessage:(NSString *)message
                      initiatedByFrame:(WKFrameInfo *)frame
                     completionHandler:(void (^)(void))completionHandler {
    NSAlert *alert = [[NSAlert alloc] init];
    alert.messageText = @"Relay";
    alert.informativeText = message;
    [alert addButtonWithTitle:@"OK"];
    [alert beginSheetModalForWindow:self.window completionHandler:^(NSModalResponse _) {
        completionHandler();
    }];
}

- (void)webView:(WKWebView *)webView
    runJavaScriptConfirmPanelWithMessage:(NSString *)message
                        initiatedByFrame:(WKFrameInfo *)frame
                       completionHandler:(void (^)(BOOL result))completionHandler {
    NSAlert *alert = [[NSAlert alloc] init];
    alert.messageText = @"Relay";
    alert.informativeText = message;
    [alert addButtonWithTitle:@"OK"];
    [alert addButtonWithTitle:@"Cancel"];
    [alert beginSheetModalForWindow:self.window completionHandler:^(NSModalResponse response) {
        completionHandler(response == NSAlertFirstButtonReturn);
    }];
}

- (void)webView:(WKWebView *)webView
    runJavaScriptTextInputPanelWithPrompt:(NSString *)prompt
                              defaultText:(NSString *)defaultText
                         initiatedByFrame:(WKFrameInfo *)frame
                        completionHandler:(void (^)(NSString * _Nullable result))completionHandler {
    NSAlert *alert = [[NSAlert alloc] init];
    alert.messageText = @"Relay";
    alert.informativeText = prompt;
    [alert addButtonWithTitle:@"OK"];
    [alert addButtonWithTitle:@"Cancel"];
    NSTextField *input = [[NSTextField alloc] initWithFrame:NSMakeRect(0, 0, 280, 24)];
    input.stringValue = defaultText ?: @"";
    alert.accessoryView = input;
    [alert beginSheetModalForWindow:self.window completionHandler:^(NSModalResponse response) {
        completionHandler(response == NSAlertFirstButtonReturn ? input.stringValue : nil);
    }];
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
    //
    // Disable macOS text substitutions inside any text field we host.
    // WKWebView text inputs inherit these from NSTextInputContext, and the
    // dash substitution silently turns command-line args like
    // "--dangerously-skip-permissions" into "—dangerously-skip-permissions"
    // (em dash), breaking flag parsing for spawned binaries.
    [[NSUserDefaults standardUserDefaults] registerDefaults:@{
        @"NSAutomaticDashSubstitutionEnabled":     @NO,
        @"NSAutomaticQuoteSubstitutionEnabled":    @NO,
        @"NSAutomaticPeriodSubstitutionEnabled":   @NO,
        @"NSAutomaticTextReplacementEnabled":      @NO,
        @"NSAutomaticSpellingCorrectionEnabled":   @NO,
        @"NSAutomaticCapitalizationEnabled":       @NO,
    }];
}
- (void)applicationWillTerminate:(NSNotification *)notification {
    goOnAppTerminate();
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
// Toggle row view — draws hover highlight like native menu items
// ---------------------------------------------------------------------------
@interface ToggleRowView : NSView
@end

@implementation ToggleRowView

- (BOOL)allowsVibrancy { return NO; }

- (void)drawRect:(NSRect)dirtyRect {
    if (self.enclosingMenuItem.isHighlighted) {
        NSRect inset = NSInsetRect(self.bounds, 4.0, 1.0);
        NSBezierPath *path = [NSBezierPath bezierPathWithRoundedRect:inset
                                                             xRadius:4.0 yRadius:4.0];
        [[NSColor selectedContentBackgroundColor] set];
        [path fill];
    }
}

- (void)viewDidMoveToWindow {
    [super viewDidMoveToWindow];
    // Remove stale tracking areas
    for (NSTrackingArea *area in self.trackingAreas) {
        [self removeTrackingArea:area];
    }
    if (self.window) {
        NSTrackingArea *area = [[NSTrackingArea alloc]
            initWithRect:NSZeroRect
                 options:(NSTrackingMouseEnteredAndExited |
                          NSTrackingActiveInActiveApp |
                          NSTrackingInVisibleRect)
                   owner:self
                userInfo:nil];
        [self addTrackingArea:area];
    }
}

- (void)mouseEntered:(NSEvent *)event { [self setNeedsDisplay:YES]; }
- (void)mouseExited:(NSEvent *)event  { [self setNeedsDisplay:YES]; }
- (void)mouseUp:(NSEvent *)event      { /* absorb so menu stays open */ }
@end

// ---------------------------------------------------------------------------
// Custom toggle switch — draws a pill-shaped on/off control
// ---------------------------------------------------------------------------
@interface ToggleSwitch : NSControl
@property (nonatomic) BOOL on;
@end

@implementation ToggleSwitch

- (instancetype)initWithFrame:(NSRect)frame {
    self = [super initWithFrame:frame];
    if (self) { _on = NO; }
    return self;
}

- (void)drawRect:(NSRect)dirtyRect {
    NSRect bounds = self.bounds;
    CGFloat h = bounds.size.height;
    CGFloat r = h / 2.0;

    // Track
    NSBezierPath *track = [NSBezierPath bezierPathWithRoundedRect:bounds
                                                          xRadius:r yRadius:r];
    if (self.on) {
        [[NSColor controlAccentColor] set];
    } else {
        [[NSColor secondaryLabelColor] set];
    }
    [track fill];

    // Knob
    CGFloat inset = 2.0;
    CGFloat knobD = h - inset * 2;
    CGFloat knobX = self.on ? (bounds.size.width - knobD - inset) : inset;
    NSBezierPath *knob = [NSBezierPath bezierPathWithOvalInRect:
        NSMakeRect(knobX, inset, knobD, knobD)];
    [[NSColor whiteColor] set];
    [knob fill];
}

- (void)mouseDown:(NSEvent *)event {
    [self sendAction:self.action to:self.target];
}

@end

// ---------------------------------------------------------------------------
// Toggle action target
// ---------------------------------------------------------------------------
@interface ToggleTarget : NSObject
- (void)toggleChanged:(ToggleSwitch *)sender;
@end

static ToggleTarget *toggleTarget = nil;

@implementation ToggleTarget
- (void)toggleChanged:(ToggleSwitch *)sender {
    int itemID = (int)sender.tag;
    goOnMenuClick(itemID);
}
@end

// ---------------------------------------------------------------------------
// Label click target (URL service labels)
// ---------------------------------------------------------------------------
@interface LabelClickTarget : NSObject
- (void)labelClicked:(NSButton *)sender;
@end

static LabelClickTarget *labelClickTarget = nil;

@implementation LabelClickTarget
- (void)labelClicked:(NSButton *)sender {
    NSString *url = sender.toolTip;  // URL stored in toolTip
    if (url && url.length > 0) {
        [[NSWorkspace sharedWorkspace] openURL:[NSURL URLWithString:url]];
        [appDelegate.statusItem.menu cancelTrackingWithoutAnimation];
    }
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
    toggleTarget = [[ToggleTarget alloc] init];
    labelClickTarget = [[LabelClickTarget alloc] init];

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

        NSNumber *isToggle = item[@"toggle"];
        if (isToggle && [isToggle boolValue]) {
            NSNumber *isOn = item[@"on"];
            NSString *url = item[@"url"];
            NSString *aux = item[@"aux"];

            CGFloat viewWidth = 290.0;
            CGFloat viewHeight = 30.0;
            CGFloat hPad = 14.0;
            CGFloat switchLabelGap = 8.0;
            CGFloat auxGap = 12.0;
            NSFont *menuFont = [NSFont menuFontOfSize:0];

            ToggleRowView *rowView = [[ToggleRowView alloc] initWithFrame:
                NSMakeRect(0, 0, viewWidth, viewHeight)];

            // Custom toggle switch
            CGFloat swW = 36.0, swH = 20.0;
            ToggleSwitch *toggle = [[ToggleSwitch alloc] initWithFrame:
                NSMakeRect(hPad, round((viewHeight - swH) / 2.0), swW, swH)];
            toggle.on = (isOn && [isOn boolValue]);
            toggle.target = toggleTarget;
            toggle.action = @selector(toggleChanged:);
            toggle.tag = [itemID integerValue];
            [rowView addSubview:toggle];

            CGFloat labelX = NSMaxX(toggle.frame) + switchLabelGap;

            // Tabular digits so the aux value doesn't kern as it changes.
            CGFloat labelRightBound = viewWidth - hPad;
            if (aux.length > 0) {
                NSTextField *auxLabel = [NSTextField labelWithString:aux];
                auxLabel.font = [NSFont monospacedDigitSystemFontOfSize:11
                                                                 weight:NSFontWeightRegular];
                auxLabel.textColor = [NSColor secondaryLabelColor];
                auxLabel.alignment = NSTextAlignmentRight;
                [auxLabel sizeToFit];
                NSRect auxFrame = auxLabel.frame;
                auxFrame.origin.x = viewWidth - hPad - auxFrame.size.width;
                auxFrame.origin.y = (viewHeight - auxFrame.size.height) / 2.0;
                auxLabel.frame = auxFrame;
                [rowView addSubview:auxLabel];
                labelRightBound = auxFrame.origin.x - auxGap;
            }

            CGFloat maxLabelWidth = labelRightBound - labelX;

            if (url && url.length > 0) {
                // Clickable label for URL services
                NSButton *btn = [NSButton buttonWithTitle:title
                                                   target:labelClickTarget
                                                   action:@selector(labelClicked:)];
                btn.bordered = NO;
                btn.font = menuFont;
                btn.contentTintColor = [NSColor labelColor];
                btn.toolTip = url;
                [[btn cell] setLineBreakMode:NSLineBreakByTruncatingTail];
                [btn sizeToFit];
                NSRect btnFrame = btn.frame;
                if (btnFrame.size.width > maxLabelWidth) {
                    btnFrame.size.width = maxLabelWidth;
                }
                btnFrame.origin.x = labelX;
                btnFrame.origin.y = (viewHeight - btnFrame.size.height) / 2.0;
                btn.frame = btnFrame;
                [rowView addSubview:btn];
            } else {
                // Plain label for non-URL services
                NSTextField *label = [NSTextField labelWithString:title];
                label.font = menuFont;
                label.textColor = [NSColor labelColor];
                label.lineBreakMode = NSLineBreakByTruncatingTail;
                [label sizeToFit];
                NSRect labelFrame = label.frame;
                if (labelFrame.size.width > maxLabelWidth) {
                    labelFrame.size.width = maxLabelWidth;
                }
                labelFrame.origin.x = labelX;
                labelFrame.origin.y = (viewHeight - labelFrame.size.height) / 2.0;
                label.frame = labelFrame;
                [rowView addSubview:label];
            }

            NSMenuItem *mi = [[NSMenuItem alloc] initWithTitle:@""
                                                        action:nil
                                                 keyEquivalent:@""];
            mi.view = rowView;
            mi.tag = [itemID integerValue];
            [menu addItem:mi];
            continue;
        }

        // Standard menu item (Settings, Exit, etc.)
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
    // SettingsWindowController implements WKUIDelegate so JS alert / confirm /
    // prompt actually surface as NSAlert sheets (default is silent no-op).
    webView.UIDelegate = settingsCtrl;
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
// cocoa_open_url
// ---------------------------------------------------------------------------
void cocoa_open_url(const char* url) {
    NSString *urlStr = [NSString stringWithUTF8String:url];
    [[NSWorkspace sharedWorkspace] openURL:[NSURL URLWithString:urlStr]];
}

// ---------------------------------------------------------------------------
// cocoa_dispatch_main_callback
// ---------------------------------------------------------------------------
extern void goDispatchCallback(uintptr_t ctx);

void cocoa_dispatch_main_callback(uintptr_t ctx) {
    dispatch_async(dispatch_get_main_queue(), ^{
        goDispatchCallback(ctx);
    });
}

// ---------------------------------------------------------------------------
// TCC permission primers
// ---------------------------------------------------------------------------
// macOS suppresses TCC prompts for background apps spawned by other background
// apps (the macmcp-from-relay-tray chain). To work around this, Relay itself
// requests Calendar/Contacts/Reminders access from its own /Applications-resident
// process. The prompt fires labeled "Relay wants to access X", the user grants
// it once, and macMCP (spawned as a relay subprocess) inherits via TCC's
// responsible-parent attribution.
//
// IMPORTANT: these block the calling thread (typically a Go goroutine via cgo),
// NOT the main thread. The completion handlers fire on an arbitrary queue and
// signal the semaphore. Main thread must keep running NSApp's event loop so
// the prompt UI actually renders -- which is fine because cocoa_run_app is
// already there blocking on [NSApp run].

static int wait_for_completion(dispatch_semaphore_t sem, int timeoutSec) {
    dispatch_time_t deadline = dispatch_time(DISPATCH_TIME_NOW, (int64_t)timeoutSec * NSEC_PER_SEC);
    return dispatch_semaphore_wait(sem, deadline) == 0 ? 1 : 0;
}

// Saved activation policy; restored by cocoa_end_foreground_activation.
// Guarded by being called only from main thread (via dispatch_sync below),
// so no atomicity needed.
static NSApplicationActivationPolicy gSavedPolicy = NSApplicationActivationPolicyAccessory;

void cocoa_begin_foreground_activation(void) {
    dispatch_sync(dispatch_get_main_queue(), ^{
        gSavedPolicy = [[NSApplication sharedApplication] activationPolicy];
        [[NSApplication sharedApplication] setActivationPolicy:NSApplicationActivationPolicyRegular];
        [[NSApplication sharedApplication] activateIgnoringOtherApps:YES];
    });
}

void cocoa_end_foreground_activation(void) {
    dispatch_sync(dispatch_get_main_queue(), ^{
        [[NSApplication sharedApplication] setActivationPolicy:gSavedPolicy];
    });
}

int cocoa_request_tcc_calendar(int timeoutSec) {
    EKAuthorizationStatus status = [EKEventStore authorizationStatusForEntityType:EKEntityTypeEvent];
    NSLog(@"relay-tcc: calendar request entered, status=%ld", (long)status);
    if (status == EKAuthorizationStatusFullAccess || status == EKAuthorizationStatusAuthorized) return 1;
    if (status != EKAuthorizationStatusNotDetermined) {
        NSLog(@"relay-tcc: calendar status is non-prompting (%ld), returning 0", (long)status);
        return 0;
    }

    EKEventStore *store = [[EKEventStore alloc] init];
    __block BOOL ok = NO;
    __block BOOL completed = NO;
    dispatch_semaphore_t sem = dispatch_semaphore_create(0);
    NSLog(@"relay-tcc: calendar calling requestFullAccessToEvents...");
    if (@available(macOS 14.0, *)) {
        [store requestFullAccessToEventsWithCompletion:^(BOOL granted, NSError *err) {
            ok = granted;
            completed = YES;
            NSLog(@"relay-tcc: calendar completion fired, granted=%d, err=%@", granted, err);
            dispatch_semaphore_signal(sem);
        }];
    } else {
        [store requestAccessToEntityType:EKEntityTypeEvent completion:^(BOOL granted, NSError *err) {
            ok = granted;
            completed = YES;
            NSLog(@"relay-tcc: calendar (legacy) completion fired, granted=%d, err=%@", granted, err);
            dispatch_semaphore_signal(sem);
        }];
    }
    int signaled = wait_for_completion(sem, timeoutSec);
    NSLog(@"relay-tcc: calendar wait returned signaled=%d completed=%d ok=%d", signaled, completed, ok);
    if (!signaled) return 0;
    return ok ? 1 : 0;
}

int cocoa_request_tcc_reminders(int timeoutSec) {
    EKAuthorizationStatus status = [EKEventStore authorizationStatusForEntityType:EKEntityTypeReminder];
    if (status == EKAuthorizationStatusFullAccess || status == EKAuthorizationStatusAuthorized) return 1;
    if (status != EKAuthorizationStatusNotDetermined) return 0;

    EKEventStore *store = [[EKEventStore alloc] init];
    __block BOOL ok = NO;
    dispatch_semaphore_t sem = dispatch_semaphore_create(0);
    if (@available(macOS 14.0, *)) {
        [store requestFullAccessToRemindersWithCompletion:^(BOOL granted, NSError *_) {
            ok = granted;
            dispatch_semaphore_signal(sem);
        }];
    } else {
        [store requestAccessToEntityType:EKEntityTypeReminder completion:^(BOOL granted, NSError *_) {
            ok = granted;
            dispatch_semaphore_signal(sem);
        }];
    }
    if (!wait_for_completion(sem, timeoutSec)) return 0;
    return ok ? 1 : 0;
}

int cocoa_request_tcc_contacts(int timeoutSec) {
    CNAuthorizationStatus status = [CNContactStore authorizationStatusForEntityType:CNEntityTypeContacts];
    NSLog(@"relay-tcc: contacts request entered, status=%ld", (long)status);
    if (status == CNAuthorizationStatusAuthorized) return 1;
    if (status != CNAuthorizationStatusNotDetermined) {
        NSLog(@"relay-tcc: contacts status is non-prompting (%ld), returning 0", (long)status);
        return 0;
    }

    CNContactStore *store = [[CNContactStore alloc] init];
    __block BOOL ok = NO;
    __block BOOL completed = NO;
    dispatch_semaphore_t sem = dispatch_semaphore_create(0);
    NSLog(@"relay-tcc: contacts calling requestAccessForEntityType...");
    [store requestAccessForEntityType:CNEntityTypeContacts completionHandler:^(BOOL granted, NSError *err) {
        ok = granted;
        completed = YES;
        NSLog(@"relay-tcc: contacts completion fired, granted=%d, err=%@", granted, err);
        dispatch_semaphore_signal(sem);
    }];
    int signaled = wait_for_completion(sem, timeoutSec);
    NSLog(@"relay-tcc: contacts wait returned signaled=%d completed=%d ok=%d", signaled, completed, ok);
    if (!signaled) return 0;
    return ok ? 1 : 0;
}

