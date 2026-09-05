#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#import <EventKit/EventKit.h>
#import <Contacts/Contacts.h>
#import <UserNotifications/UserNotifications.h>
#include "cocoa_darwin.h"
#include <stdlib.h>

extern void goOnMenuClick(int itemID);
extern void goOnSettingsIpc(const char* msg);
extern void goOnSettingsClose(void);
extern void goOnAppTerminate(void);

@interface IpcHandler : NSObject <WKScriptMessageHandler>
@end

@implementation IpcHandler
- (void)userContentController:(WKUserContentController *)uc
      didReceiveScriptMessage:(WKScriptMessage *)message {
    NSString *body = (NSString *)message.body;
    goOnSettingsIpc([body UTF8String]);
}
@end

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

// WKWebView answers window.alert/confirm/prompt with a silent no-op unless
// the host implements these. Without them every confirm() guard in the
// settings UI runs as if the user had clicked "Cancel".

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

@interface AppDelegate : NSObject <NSApplicationDelegate>
@property (strong) NSStatusItem *statusItem;
@end

@implementation AppDelegate
- (void)applicationDidFinishLaunching:(NSNotification *)notification {
// WKWebView text inputs inherit NSTextInputContext's substitutions, and dash
// substitution rewrites "--dangerously-skip-permissions" with an em dash,
// breaking flag parsing for binaries spawned from here.
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

static AppDelegate *appDelegate = nil;

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

    NSBezierPath *track = [NSBezierPath bezierPathWithRoundedRect:bounds
                                                          xRadius:r yRadius:r];
    if (self.on) {
        [[NSColor controlAccentColor] set];
    } else {
        [[NSColor secondaryLabelColor] set];
    }
    [track fill];

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

void cocoa_init_app(void) {
    [NSApplication sharedApplication];
    [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];

    appDelegate = [[AppDelegate alloc] init];
    [NSApp setDelegate:appDelegate];

    menuTarget = [[MenuTarget alloc] init];
    toggleTarget = [[ToggleTarget alloc] init];
    labelClickTarget = [[LabelClickTarget alloc] init];

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

void cocoa_run_app(void) {
    [NSApp run];
}

void cocoa_setup_tray(const unsigned char* iconRGBA, int width, int height) {
    NSStatusBar *bar = [NSStatusBar systemStatusBar];
    appDelegate.statusItem = [bar statusItemWithLength:NSVariableStatusItemLength];

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

    NSMenu *menu = [[NSMenu alloc] init];
    appDelegate.statusItem.menu = menu;
}

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

        NSString *key = item[@"key"];
        NSMenuItem *mi = [[NSMenuItem alloc] initWithTitle:title
                                                    action:@selector(menuItemClicked:)
                                             keyEquivalent:(key.length > 0 ? key : @"")];
        mi.target = menuTarget;
        mi.tag = [itemID integerValue];
        mi.enabled = enabled ? [enabled boolValue] : YES;
        [menu addItem:mi];
    }
}

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

    WKWebViewConfiguration *config = [[WKWebViewConfiguration alloc] init];
    WKUserContentController *uc = [[WKUserContentController alloc] init];
    settingsCtrl.ipcHandler = [[IpcHandler alloc] init];
    [uc addScriptMessageHandler:settingsCtrl.ipcHandler name:@"ipc"];
    config.userContentController = uc;

    WKWebView *webView = [[WKWebView alloc] initWithFrame:window.contentView.bounds configuration:config];
    webView.autoresizingMask = NSViewWidthSizable | NSViewHeightSizable;
    webView.UIDelegate = settingsCtrl;
    settingsCtrl.webView = webView;

    // Left nil deliberately: an unpinned appearance inherits the window's
    // effectiveAppearance, which tracks the system. underPageBackgroundColor
    // is the adaptive backdrop, so a dark system gets no white flash.
    if (@available(macOS 12.0, *)) {
        webView.underPageBackgroundColor = [NSColor windowBackgroundColor];
    }

    [window.contentView addSubview:webView];

    NSString *htmlStr = [NSString stringWithUTF8String:html];
    [webView loadHTMLString:htmlStr baseURL:nil];

    [window makeKeyAndOrderFront:nil];
    [NSApp activateIgnoringOtherApps:YES];
}

void cocoa_settings_eval_js(const char* js) {
    if (!settingsCtrl || !settingsCtrl.webView) return;
    NSString *script = [NSString stringWithUTF8String:js];
    [settingsCtrl.webView evaluateJavaScript:script completionHandler:nil];
}

void cocoa_open_url(const char* url) {
    NSString *urlStr = [NSString stringWithUTF8String:url];
    [[NSWorkspace sharedWorkspace] openURL:[NSURL URLWithString:urlStr]];
}

// ---------------------------------------------------------------------------
// User notifications
// ---------------------------------------------------------------------------

extern void goOnNotificationClick(void);
extern void goOnNotificationsDenied(const char* detail);

// Every delivery reuses this identifier so a second notification REPLACES
// the first in Notification Center instead of stacking — coalescing at the
// OS layer, on top of the coalescing pendingEnrolmentNotifier already does.
static NSString *const kRelayPendingEnrolmentNotificationID = @"relay.enrolment.pending";

@interface NotificationDelegate : NSObject <UNUserNotificationCenterDelegate>
@end

static NotificationDelegate *notificationDelegate = nil;

@implementation NotificationDelegate
- (void)userNotificationCenter:(UNUserNotificationCenter *)center
       didReceiveNotificationResponse:(UNNotificationResponse *)response
                withCompletionHandler:(void (^)(void))completionHandler {
    dispatch_async(dispatch_get_main_queue(), ^{
        goOnNotificationClick();
    });
    completionHandler();
}

// Without this the banner is suppressed whenever Relay is the frontmost
// app, which is exactly the case while the operator is clicking through the
// tray menu that raised it.
- (void)userNotificationCenter:(UNUserNotificationCenter *)center
       willPresentNotification:(UNNotification *)notification
         withCompletionHandler:(void (^)(UNNotificationPresentationOptions))completionHandler {
    completionHandler(UNNotificationPresentationOptionList | UNNotificationPresentationOptionBanner);
}
@end

// UNUserNotificationCenter.currentNotificationCenter THROWS for a process
// with no bundle identifier, and relay runs bare from a terminal during
// development and under go test. Guarding here — rather than letting the
// exception escape into Go — is what keeps `./relay` runnable.
static UNUserNotificationCenter *relay_notification_center(void) {
    static BOOL warned = NO;
    if ([[NSBundle mainBundle] bundleIdentifier] == nil) {
        if (!warned) {
            warned = YES;
            NSLog(@"relay: no bundle identifier, user notifications unavailable");
        }
        return nil;
    }
    return [UNUserNotificationCenter currentNotificationCenter];
}

typedef enum {
    kRelayAuthUnasked = 0,
    kRelayAuthAsking,
    kRelayAuthAnswered,
} RelayAuthState;

// Both are read and written only on the main thread: cocoa_notify is reached
// from the tray's menu rebuild, which is always dispatched to main, and the
// authorization completion handler — which fires on an arbitrary queue —
// hops to main before touching them. Hence unguarded.
//
// gDeferred holds at most one request: the newest raised while the
// authorization answer is still outstanding. A request added before the
// answer arrives is discarded by macOS, and every delivery shares one
// identifier anyway, so only the freshest is worth holding. It is an array
// rather than a plain pointer because this file is compiled without ARC: the
// array owns what it holds, so nothing here has to say retain or release.
static RelayAuthState gAuthState = kRelayAuthUnasked;
static NSMutableArray *gDeferred = nil;

void cocoa_notify(const char* title, const char* body) {
    UNUserNotificationCenter *center = relay_notification_center();
    if (center == nil) return;

    if (notificationDelegate == nil) {
        notificationDelegate = [[NotificationDelegate alloc] init];
        center.delegate = notificationDelegate;
    }

    UNMutableNotificationContent *content = [[UNMutableNotificationContent alloc] init];
    content.title = [NSString stringWithUTF8String:title];
    content.body = [NSString stringWithUTF8String:body];

    UNNotificationRequest *req =
        [UNNotificationRequest requestWithIdentifier:kRelayPendingEnrolmentNotificationID
                                             content:content
                                             trigger:nil];

    if (gAuthState == kRelayAuthAsking) {
        [gDeferred removeAllObjects];
        [gDeferred addObject:req];
        return;
    }

    // Authorization is requested once, lazily, on the first notification —
    // never at launch, where a permission prompt for a feature the operator
    // may never turn on is noise.
    if (gAuthState == kRelayAuthUnasked) {
        gAuthState = kRelayAuthAsking;
        gDeferred = [[NSMutableArray alloc] initWithObjects:req, nil];
        [center requestAuthorizationWithOptions:UNAuthorizationOptionAlert
                              completionHandler:^(BOOL granted, NSError *error) {
            dispatch_async(dispatch_get_main_queue(), ^{
                gAuthState = kRelayAuthAnswered;
                if (!granted) {
                    // Silently denied is the case this exists for: macOS
                    // refuses an ad-hoc-signed, non-notarised LSUIElement
                    // bundle without ever prompting, and nothing else in the
                    // system says so.
                    NSString *detail = error.localizedDescription;
                    goOnNotificationsDenied(detail ? [detail UTF8String] : "");
                } else if (gDeferred.count > 0) {
                    // Read before the clear: the array is the only owner.
                    [center addNotificationRequest:gDeferred.lastObject
                             withCompletionHandler:nil];
                }
                [gDeferred removeAllObjects];
            });
        }];
        return;
    }

    [center addNotificationRequest:req withCompletionHandler:nil];
}

extern void goDispatchCallback(uintptr_t ctx);


void cocoa_dispatch_main_callback(uintptr_t ctx) {
    dispatch_async(dispatch_get_main_queue(), ^{
        goDispatchCallback(ctx);
    });
}

// macOS suppresses TCC prompts for a background app spawned by another
// background app, so Relay requests these from its own bundle and the MCPs
// it spawns inherit the grant by responsible-parent attribution.
//
// These block the CALLING thread, not the main one: completion handlers fire
// on an arbitrary queue and signal the semaphore, while the main thread must
// stay in [NSApp run] for the prompt to render at all.

static int wait_for_completion(dispatch_semaphore_t sem, int timeoutSec) {
    dispatch_time_t deadline = dispatch_time(DISPATCH_TIME_NOW, (int64_t)timeoutSec * NSEC_PER_SEC);
    return dispatch_semaphore_wait(sem, deadline) == 0 ? 1 : 0;
}

// Only ever touched from the main thread via dispatch_sync, hence unguarded.
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
    if (status == EKAuthorizationStatusFullAccess || status == EKAuthorizationStatusAuthorized) return 1;
    if (status != EKAuthorizationStatusNotDetermined) return 0;

    EKEventStore *store = [[EKEventStore alloc] init];
    __block BOOL ok = NO;
    dispatch_semaphore_t sem = dispatch_semaphore_create(0);
    if (@available(macOS 14.0, *)) {
        [store requestFullAccessToEventsWithCompletion:^(BOOL granted, NSError *_) {
            ok = granted;
            dispatch_semaphore_signal(sem);
        }];
    } else {
        [store requestAccessToEntityType:EKEntityTypeEvent completion:^(BOOL granted, NSError *_) {
            ok = granted;
            dispatch_semaphore_signal(sem);
        }];
    }
    if (!wait_for_completion(sem, timeoutSec)) return 0;
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
    if (status == CNAuthorizationStatusAuthorized) return 1;
    if (status != CNAuthorizationStatusNotDetermined) return 0;

    CNContactStore *store = [[CNContactStore alloc] init];
    __block BOOL ok = NO;
    dispatch_semaphore_t sem = dispatch_semaphore_create(0);
    [store requestAccessForEntityType:CNEntityTypeContacts completionHandler:^(BOOL granted, NSError *_) {
        ok = granted;
        dispatch_semaphore_signal(sem);
    }];
    if (!wait_for_completion(sem, timeoutSec)) return 0;
    return ok ? 1 : 0;
}
