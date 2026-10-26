#import <Cocoa/Cocoa.h>
#include "_cgo_export.h"


@interface FioTrayTarget : NSObject
- (void)act:(NSMenuItem *)sender;
@end
@implementation FioTrayTarget
- (void)act:(NSMenuItem *)sender { goTrayAction((int)sender.tag); }
@end

static NSStatusItem *fioItem;
static NSMenuItem *fioStatusLine;
static NSMenuItem *fioLoginItem;
static NSMenuItem *fioUpdateItem;
static NSMenuItem *fioCheckItem;
static FioTrayTarget *fioTarget;

BOOL fioQuickVisible(void); // quick_darwin.m

static NSMenuItem *fioAdd(NSMenu *m, NSString *title, int tag, NSString *key) {
	NSMenuItem *it = [[NSMenuItem alloc] initWithTitle:title action:@selector(act:) keyEquivalent:key];
	it.target = fioTarget;
	it.tag = tag;
	[m addItem:it];
	return it;
}

// fioAppMenuAdd puts "Check for Updates…" under About in the app menu, which
// Wails builds with no way to add items. It's set before the run loop starts,
// so it's normally there already; tries is how many more times to look.
static void fioAppMenuAdd(int tries) {
	NSMenu *app = NSApp.mainMenu.itemArray.firstObject.submenu;
	if (!app) {
		if (tries > 0) dispatch_after(dispatch_time(DISPATCH_TIME_NOW, NSEC_PER_SEC / 2), dispatch_get_main_queue(), ^{ fioAppMenuAdd(tries - 1); });
		return;
	}
	if ([app itemWithTag:6]) return;
	NSMenuItem *it = [[NSMenuItem alloc] initWithTitle:@"Check for Updates…" action:@selector(act:) keyEquivalent:@""];
	it.target = fioTarget;
	it.tag = 6;
	if ([app.itemArray.firstObject.title hasPrefix:@"About "]) {
		[app insertItem:it atIndex:1];
	} else {
		[app insertItem:[NSMenuItem separatorItem] atIndex:0];
		[app insertItem:it atIndex:0];
	}
}

void fioTrayInit(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		if (fioItem) return;
		fioTarget = [FioTrayTarget new];
		fioItem = [[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength];
		NSImage *img = [NSImage imageWithSystemSymbolName:@"arrow.up.arrow.down" accessibilityDescription:@"FioTransfer"];
		img.template = YES;
		fioItem.button.image = img;
		fioItem.button.imagePosition = NSImageLeft;
		NSMenu *m = [NSMenu new];
		m.autoenablesItems = NO;
		fioStatusLine = [[NSMenuItem alloc] initWithTitle:@"Idle" action:nil keyEquivalent:@""];
		fioStatusLine.enabled = NO;
		[m addItem:fioStatusLine];
		[m addItem:[NSMenuItem separatorItem]];
		fioUpdateItem = fioAdd(m, @"Update", 5, @"");
		fioUpdateItem.hidden = YES;
		fioAdd(m, @"Download from Link…", 7, @"");
		fioAdd(m, @"Open FioTransfer", 1, @"");
		fioLoginItem = fioAdd(m, @"Open at Login", 2, @"");
		fioCheckItem = fioAdd(m, @"Check for Updates…", 6, @"");
		[m addItem:[NSMenuItem separatorItem]];
		fioAdd(m, @"Quit FioTransfer", 3, @"q");
		fioItem.menu = m;
		fioAppMenuAdd(5);
		// Re-opening the app (e.g. double-click in Finder) while hidden brings the window back.
		[[NSNotificationCenter defaultCenter] addObserverForName:NSApplicationDidBecomeActiveNotification object:nil queue:nil usingBlock:^(NSNotification *n) {
			if (fioQuickVisible()) return;
			for (NSWindow *w in NSApp.windows) {
				if (w.isVisible && w.canBecomeMainWindow) return;
			}
			goTrayAction(4);
		}];
	});
}

void fioTraySet(const char *status, const char *title, int login, const char *update) {
	NSString *s = [NSString stringWithUTF8String:status];
	NSString *t = [NSString stringWithUTF8String:title];
	NSString *u = [NSString stringWithUTF8String:update];
	dispatch_async(dispatch_get_main_queue(), ^{
		if (!fioItem) return;
		fioStatusLine.title = s;
		fioItem.button.title = t;
		fioLoginItem.state = login ? NSControlStateValueOn : NSControlStateValueOff;
		fioUpdateItem.hidden = u.length == 0;
		fioCheckItem.hidden = u.length > 0; // the update item takes its place
		if (u.length) fioUpdateItem.title = u;
	});
}

// Show the Dock icon only while a window is on screen.
void fioSyncDock(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		BOOL visible = NO;
		for (NSWindow *w in NSApp.windows) {
			if ((w.isVisible || w.isMiniaturized) && w.canBecomeMainWindow) { visible = YES; break; }
		}
		NSApplicationActivationPolicy want = visible ? NSApplicationActivationPolicyRegular : NSApplicationActivationPolicyAccessory;
		if (NSApp.activationPolicy != want) [NSApp setActivationPolicy:want];
	});
}

void fioActivate(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		[NSApp setActivationPolicy:NSApplicationActivationPolicyRegular];
		[NSApp activateIgnoringOtherApps:YES];
	});
}

// While transfers run, tell macOS this is user-requested work: no App Nap
// throttling and no idle sleep, even when the window is hidden.
static id fioActivity;

void fioSetActive(int on) {
	dispatch_async(dispatch_get_main_queue(), ^{
		if (on && !fioActivity) {
			fioActivity = [[NSProcessInfo processInfo] beginActivityWithOptions:(NSActivityUserInitiated | NSActivityIdleSystemSleepDisabled)
			                                                             reason:@"Transfers in progress"];
		} else if (!on && fioActivity) {
			[[NSProcessInfo processInfo] endActivity:fioActivity];
			fioActivity = nil;
		}
	});
}
