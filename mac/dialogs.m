// Native panels and alerts (see src/dialogs.rs).
//
// Wails answered the Go app's OpenFileDialog / SaveFileDialog / MessageDialog /
// ClipboardGetText / ClipboardSetText calls; this file is their AppKit
// replacement, plus the front-window PNG the measurement harness takes.
//
// Every function here is synchronous, and AppKit only wants panels on the main
// thread while the Rust callers may be tokio workers: each function hops to the
// main queue and waits when it is called from anywhere else (fioOnMain). On the
// main thread the panel runs inline, because dispatch_sync to the main queue
// from the main thread would deadlock. A dialog asked for before the app's
// event loop is running has no main queue to answer on and is answered as
// cancelled instead of hanging.
//
// Strings cross the boundary as UTF-8 C strings that the caller frees with
// fioFree, except the file list, which is JSON so one string carries many paths.

#import <Cocoa/Cocoa.h>
#include <dlfcn.h>
#include <stdlib.h>
#include <string.h>

// ---------- Strings ----------

// fioStr reads an argument (NULL reads as empty).
static NSString *fioStr(const char *s) {
	if (!s) return @"";
	NSString *r = [NSString stringWithUTF8String:s];
	return r ? r : @"";
}

// fioCopy hands a string to Rust: malloc'd UTF-8, freed with fioFree.
static char *fioCopy(NSString *s) {
	const char *u = s ? s.UTF8String : "";
	if (!u) u = "";
	size_t n = strlen(u);
	char *p = (char *)malloc(n + 1);
	if (!p) return NULL;
	memcpy(p, u, n + 1);
	return p;
}

void fioFree(void *p) {
	free(p);
}

// ---------- The main thread ----------

// fioOnMain runs work on the main thread and waits for it. Off the main thread
// with no event loop yet, there is nothing to answer on: the work is skipped
// rather than waiting for a queue that never drains.
static BOOL fioOnMain(dispatch_block_t work) {
	if ([NSThread isMainThread]) {
		work();
		return YES;
	}
	if (!NSApp || ![NSApp isRunning]) {
		NSLog(@"FioTransfer: a dialog was asked for before the app started");
		return NO;
	}
	dispatch_sync(dispatch_get_main_queue(), work);
	return YES;
}

// fioForward brings the panel in front of other apps, the way a click on the
// menu bar icon would.
static void fioForward(void) {
	[NSApp activateIgnoringOtherApps:YES];
}

// ---------- Panels ----------

// The title a panel shows is the app's name. Wails set the panel's title too,
// with the same note: valid but appears to do nothing. (Save panels are the
// base class, open panels are the subclass.)
static void fioTitle(NSSavePanel *p, const char *title) {
	NSString *t = fioStr(title);
	if (t.length) p.title = t;
}

// fioOpenFiles asks for files to upload. The answer is a JSON array of paths,
// "[]" when cancelled or when nothing could be shown.
char *fioOpenFiles(const char *title) {
	__block char *out = NULL;
	BOOL ran = fioOnMain(^{
		NSOpenPanel *p = [NSOpenPanel openPanel];
		p.canChooseFiles = YES;
		p.canChooseDirectories = NO;
		p.allowsMultipleSelection = YES;
		p.resolvesAliases = YES; // Go's dialogs passed ResolvesAliases: true
		fioTitle(p, title);
		fioForward();
		NSMutableArray<NSString *> *paths = [NSMutableArray new];
		if ([p runModal] == NSModalResponseOK) {
			for (NSURL *u in p.URLs) {
				NSString *path = u.path;
				if (path.length) [paths addObject:path];
			}
		}
		NSData *data = [NSJSONSerialization dataWithJSONObject:paths options:0 error:nil];
		NSString *json = data ? [[NSString alloc] initWithData:data encoding:NSUTF8StringEncoding] : @"[]";
		out = fioCopy(json.length ? json : @"[]");
	});
	if (!ran) return fioCopy(@"[]");
	return out;
}

// fioOpenDir asks for one folder. "" when cancelled.
char *fioOpenDir(const char *title, const char *def, int canCreate) {
	__block char *out = NULL;
	BOOL ran = fioOnMain(^{
		NSOpenPanel *p = [NSOpenPanel openPanel];
		p.canChooseFiles = NO;
		p.canChooseDirectories = YES;
		p.canCreateDirectories = canCreate != 0;
		p.allowsMultipleSelection = NO;
		p.resolvesAliases = YES; // Go's dialogs passed ResolvesAliases: true
		fioTitle(p, title);
		NSString *d = fioStr(def);
		if (d.length) p.directoryURL = [NSURL fileURLWithPath:d isDirectory:YES];
		fioForward();
		NSString *path = @"";
		if ([p runModal] == NSModalResponseOK && p.URL.path.length) path = p.URL.path;
		out = fioCopy(path);
	});
	if (!ran) return fioCopy(@"");
	return out;
}

// fioSaveFile asks where to write a file. "" when cancelled.
char *fioSaveFile(const char *title, const char *defName) {
	__block char *out = NULL;
	BOOL ran = fioOnMain(^{
		NSSavePanel *p = [NSSavePanel savePanel];
		fioTitle(p, title);
		NSString *n = fioStr(defName);
		if (n.length) p.nameFieldStringValue = n;
		fioForward();
		NSString *path = @"";
		if ([p runModal] == NSModalResponseOK && p.URL.path.length) path = p.URL.path;
		out = fioCopy(path);
	});
	if (!ran) return fioCopy(@"");
	return out;
}

// ---------- Alerts ----------

// fioQuestion asks a two-button question; 1 when `ok` was pressed.
//
// The buttons are labelled as given and the key equivalents are set the way
// Wails' WailsAlert set them, because that is what the Go app's dialogs did:
// `cancel` (the Go app's CancelButton) answers Return, `ok` gets no key of its
// own. Every Go question had Cancel as its default button ("Stop running
// transfers and quit?", "Updating restarts FioTransfer…"), so Return there
// meant Cancel, and it does here too; `ok` is clicked, or reached with Tab.
int fioQuestion(const char *title, const char *message, const char *ok, const char *cancel) {
	__block int yes = 0;
	BOOL ran = fioOnMain(^{
		NSAlert *a = [NSAlert new];
		a.alertStyle = NSAlertStyleInformational; // Wails' "question"
		a.messageText = fioStr(title);
		a.informativeText = fioStr(message);
		NSString *okTitle = fioStr(ok), *cancelTitle = fioStr(cancel);
		NSButton *okButton = [a addButtonWithTitle:okTitle.length ? okTitle : @"OK"];
		NSButton *cancelButton = [a addButtonWithTitle:cancelTitle.length ? cancelTitle : @"Cancel"];
		okButton.keyEquivalent = @"";
		cancelButton.keyEquivalent = @"\r";
		[a.window setLevel:NSFloatingWindowLevel];
		fioForward();
		yes = [a runModal] == NSAlertFirstButtonReturn ? 1 : 0;
	});
	return ran ? yes : 0;
}

// fioAlert tells the app something is wrong (Wails' "error" dialog: one OK
// button, Return on it).
void fioAlert(const char *title, const char *message) {
	fioOnMain(^{
		NSAlert *a = [NSAlert new];
		a.alertStyle = NSAlertStyleCritical;
		a.messageText = fioStr(title);
		a.informativeText = fioStr(message);
		[a addButtonWithTitle:@"OK"];
		[a.window setLevel:NSFloatingWindowLevel];
		fioForward();
		[a runModal];
	});
}

// ---------- Clipboard ----------

char *fioClipboardGet(void) {
	__block char *out = NULL;
	BOOL ran = fioOnMain(^{
		NSPasteboard *pb = [NSPasteboard generalPasteboard];
		NSString *s = [pb stringForType:NSPasteboardTypeString];
		out = fioCopy(s ? s : @"");
	});
	if (!ran) return fioCopy(@"");
	return out;
}

void fioClipboardSet(const char *t) {
	NSString *s = fioStr(t);
	fioOnMain(^{
		NSPasteboard *pb = [NSPasteboard generalPasteboard];
		[pb clearContents];
		[pb setString:s forType:NSPasteboardTypeString];
	});
}

// ---------- The front window as a PNG ----------

// fioFrontWindow is the window the user is looking at, front to back. The quick
// download panel can't become main, so it is taken last.
static NSWindow *fioFrontWindow(void) {
	NSWindow *key = NSApp.keyWindow;
	if (key) return key;
	for (NSWindow *w in [NSApp orderedWindows]) {
		if (w.isVisible && w.canBecomeMainWindow) return w;
	}
	for (NSWindow *w in [NSApp orderedWindows]) {
		if (w.isVisible) return w;
	}
	return nil;
}

// fioRenderView draws a view into a bitmap. The fallback when the window server
// won't give us the composited window.
static NSBitmapImageRep *fioRenderView(NSView *v) {
	if (!v) return nil;
	NSRect r = v.bounds;
	if (NSIsEmptyRect(r)) return nil;
	NSBitmapImageRep *rep = [v bitmapImageRepForCachingDisplayInRect:r];
	if (!rep) return nil;
	[v cacheDisplayInRect:r toBitmapImageRep:rep];
	return rep;
}

// fioWindowImage asks the window server for the window's pixels. A window of
// our own is composed for us, so this needs no screen-recording permission; the
// symbol is looked up at run time because the SDK marks it obsolete (macOS 15
// points at ScreenCaptureKit), and it can still be missing.
typedef CGImageRef (*fioWindowImageFn)(CGRect, CGWindowListOption, CGWindowID, CGWindowImageOption);

static CGImageRef fioWindowImage(NSWindow *w) {
	static fioWindowImageFn fn;
	static BOOL looked = NO;
	if (!looked) {
		looked = YES;
		*(void **)(&fn) = dlsym(RTLD_DEFAULT, "CGWindowListCreateImage");
	}
	CGWindowID n = (CGWindowID)w.windowNumber;
	if (!fn || n == 0 || !w.isVisible) return NULL;
	return fn(CGRectNull, kCGWindowListOptionIncludingWindow, n, kCGWindowImageBoundsIgnoreFraming);
}

// fioWindowPNG writes the front window as a PNG and says whether it did. The
// measurement harness reads it to check the layout it just rendered.
int fioWindowPNG(const char *path) {
	NSString *dest = fioStr(path);
	if (!dest.length) return 0;
	__block int ok = 0;
	fioOnMain(^{
		NSWindow *w = fioFrontWindow();
		if (!w) return;
		NSBitmapImageRep *rep = nil;
		CGImageRef img = fioWindowImage(w);
		if (img) {
			rep = [[NSBitmapImageRep alloc] initWithCGImage:img];
			CGImageRelease(img);
		}
		if (!rep) rep = fioRenderView(w.contentView);
		if (!rep) {
			NSLog(@"FioTransfer: could not read the front window (%@)", w.title);
			return;
		}
		NSData *png = [rep representationUsingType:NSBitmapImageFileTypePNG properties:@{}];
		if (!png) return;
		ok = [png writeToFile:dest options:NSDataWritingAtomic error:nil] ? 1 : 0;
	});
	return ok;
}
