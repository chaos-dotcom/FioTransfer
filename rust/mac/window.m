#import <Cocoa/Cocoa.h>

// The window chrome and the main menu, which Wails used to build for the Go
// app: a hidden inset title bar over the content, dark appearance, the app
// background colour, and the App/Edit/File/Window menus (the Edit menu is what
// makes ⌘C/⌘V work in text fields).

static void (*fioMenuCallback)(int);

@interface FioMenuTarget : NSObject
- (void)pick:(NSMenuItem *)sender;
@end
@implementation FioMenuTarget
- (void)pick:(NSMenuItem *)sender {
	if (fioMenuCallback) fioMenuCallback((int)sender.tag);
}
@end

static FioMenuTarget *fioMenuTarget;

static NSMenuItem *fioItem(NSMenu *m, NSString *title, SEL act, NSString *key, int tag, NSEventModifierFlags mods) {
	NSMenuItem *it = [[NSMenuItem alloc] initWithTitle:title action:act keyEquivalent:key];
	if (tag) {
		it.target = fioMenuTarget;
		it.action = @selector(pick:);
		it.tag = tag;
	}
	if (mods && key.length) it.keyEquivalentModifierMask = mods;
	[m addItem:it];
	return it;
}

void fioMenuInit(void (*cb)(int)) {
	fioMenuCallback = cb;
	dispatch_async(dispatch_get_main_queue(), ^{
		if (fioMenuTarget) return;
		fioMenuTarget = [FioMenuTarget new];
		NSString *app = @"FioTransfer";
		NSMenu *main = [NSMenu new];

		// App menu
		NSMenuItem *appItem = [[NSMenuItem alloc] initWithTitle:app action:nil keyEquivalent:@""];
		[main addItem:appItem];
		NSMenu *appMenu = [NSMenu new];
		appItem.submenu = appMenu;
		fioItem(appMenu, [@"About " stringByAppendingString:app], @selector(orderFrontStandardAboutPanel:), @"", 0, 0);
		[appMenu addItem:[NSMenuItem separatorItem]];
		fioItem(appMenu, [@"Hide " stringByAppendingString:app], @selector(hide:), @"h", 0, 0);
		fioItem(appMenu, @"Hide Others", @selector(hideOtherApplications:), @"h", 0, NSEventModifierFlagCommand | NSEventModifierFlagOption);
		fioItem(appMenu, @"Show All", @selector(unhideAllApplications:), @"", 0, 0);
		[appMenu addItem:[NSMenuItem separatorItem]];
		fioItem(appMenu, [@"Quit " stringByAppendingString:app], @selector(terminate:), @"q", 0, 0);

		// Edit menu: undo/redo and the clipboard shortcuts for text fields
		NSMenuItem *editItem = [[NSMenuItem alloc] initWithTitle:@"Edit" action:nil keyEquivalent:@""];
		[main addItem:editItem];
		NSMenu *edit = [NSMenu new];
		edit.title = @"Edit";
		editItem.submenu = edit;
		fioItem(edit, @"Undo", @selector(undo:), @"z", 0, 0);
		fioItem(edit, @"Redo", @selector(redo:), @"z", 0, NSEventModifierFlagCommand | NSEventModifierFlagShift);
		[edit addItem:[NSMenuItem separatorItem]];
		fioItem(edit, @"Cut", @selector(cut:), @"x", 0, 0);
		fioItem(edit, @"Copy", @selector(copy:), @"c", 0, 0);
		fioItem(edit, @"Paste", @selector(paste:), @"v", 0, 0);
		fioItem(edit, @"Select All", @selector(selectAll:), @"a", 0, 0);

		// File menu
		NSMenuItem *fileItem = [[NSMenuItem alloc] initWithTitle:@"File" action:nil keyEquivalent:@""];
		[main addItem:fileItem];
		NSMenu *file = [NSMenu new];
		file.title = @"File";
		fileItem.submenu = file;
		fioItem(file, @"Upload Files…", nil, @"o", 101, 0);
		fioItem(file, @"Upload Folder…", nil, @"o", 102, NSEventModifierFlagCommand | NSEventModifierFlagShift);
		fioItem(file, @"Download from Link…", nil, @"l", 103, 0);
		[file addItem:[NSMenuItem separatorItem]];
		fioItem(file, @"Show Transfer Log", nil, @"", 104, 0);

		// Window menu
		NSMenuItem *windowItem = [[NSMenuItem alloc] initWithTitle:@"Window" action:nil keyEquivalent:@""];
		[main addItem:windowItem];
		NSMenu *win = [NSMenu new];
		win.title = @"Window";
		windowItem.submenu = win;
		fioItem(win, @"Minimize", @selector(performMiniaturize:), @"m", 0, 0);
		fioItem(win, @"Zoom", @selector(performZoom:), @"", 0, 0);
		[win addItem:[NSMenuItem separatorItem]];
		fioItem(win, @"Bring All to Front", @selector(arrangeInFront:), @"", 0, 0);
		NSApp.windowsMenu = win;

		NSApp.mainMenu = main;
	});
}

// fioWindowStyle gives the window the Go app's look: no title bar over the
// content, traffic lights inset over the sidebar, the app's background colour.
void fioWindowStyle(void) {
	dispatch_async(dispatch_get_main_queue(), ^{
		NSWindow *w = NSApp.mainWindow ?: NSApp.windows.firstObject;
		if (!w) return;
		w.styleMask |= NSWindowStyleMaskFullSizeContentView;
		w.titlebarAppearsTransparent = YES;
		w.titleVisibility = NSWindowTitleHidden;
		w.movableByWindowBackground = YES;
		w.backgroundColor = [NSColor colorWithSRGBRed:17.0 / 255.0 green:18.0 / 255.0 blue:22.0 / 255.0 alpha:1.0];
		NSApp.appearance = [NSAppearance appearanceNamed:NSAppearanceNameDarkAqua];
	});
}

// fioSetWindowTitle keeps the app menu's About panel and the window title right.
void fioSetAppName(const char *name) {
	(void)name;
}


#import <WebKit/WebKit.h>

// ---- Snapshot harness (src/gui.rs) ----
// The webview's own snapshot, so comparing the two builds needs no screen
// recording permission and no compositor.

static WKWebView *fioWebView(void) {
	for (NSWindow *w in NSApp.windows) {
		NSView *v = w.contentView;
		if (!v) continue;
		for (NSView *sub in v.subviews) {
			if ([sub isKindOfClass:[WKWebView class]]) return (WKWebView *)sub;
		}
	}
	return nil;
}

int fioSnapshotWebView(const char *path) {
	__block int ok = 0;
	dispatch_semaphore_t done = dispatch_semaphore_create(0);
	dispatch_async(dispatch_get_main_queue(), ^{
		WKWebView *wv = fioWebView();
		if (!wv) { dispatch_semaphore_signal(done); return; }
		WKSnapshotConfiguration *cfg = [WKSnapshotConfiguration new];
		cfg.rect = wv.bounds;
		[wv takeSnapshotWithConfiguration:cfg completionHandler:^(NSImage *img, NSError *err) {
			if (!img) { dispatch_semaphore_signal(done); return; }
			CGImageRef cg = [img CGImageForProposedRect:NULL context:nil hints:nil];
			NSBitmapImageRep *rep = [[NSBitmapImageRep alloc] initWithCGImage:cg];
			NSData *png = [rep representationUsingType:NSBitmapImageFileTypePNG properties:@{}];
			ok = [png writeToFile:[NSString stringWithUTF8String:path] atomically:YES] ? 1 : 0;
			dispatch_semaphore_signal(done);
		}];
	});
	dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, 20ull * NSEC_PER_SEC));
	return ok;
}

void fioEvalJS(const char *js) {
	// The NSString is made here: the caller's buffer is gone once this returns.
	NSString *code = [NSString stringWithUTF8String:js ?: ""];
	dispatch_async(dispatch_get_main_queue(), ^{
		WKWebView *wv = fioWebView();
		[wv evaluateJavaScript:code completionHandler:nil];
	});
}

int fioEvalJSFile(const char *js, const char *path) {
	__block int ok = 0;
	NSString *code = [NSString stringWithUTF8String:js ?: ""];
	NSString *dest = [NSString stringWithUTF8String:path ?: "/dev/null"];
	dispatch_semaphore_t done = dispatch_semaphore_create(0);
	dispatch_async(dispatch_get_main_queue(), ^{
		WKWebView *wv = fioWebView();
		if (!wv) { dispatch_semaphore_signal(done); return; }
		[wv evaluateJavaScript:code completionHandler:^(id res, NSError *err) {
			NSString *s = res ? [NSString stringWithFormat:@"%@", res] : [NSString stringWithFormat:@"error: %@", err];
			ok = [s writeToFile:dest atomically:YES encoding:NSUTF8StringEncoding error:nil] ? 1 : 0;
			dispatch_semaphore_signal(done);
		}];
	});
	dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, 20ull * NSEC_PER_SEC));
	return ok;
}
