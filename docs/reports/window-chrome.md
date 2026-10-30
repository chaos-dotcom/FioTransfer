# The window draws its own chrome

Branch `chrome`, cut from `origin/main` at `5663a4e` ("version 1.7.0"). Worktree
`/tmp/chrome`, `CARGO_TARGET_DIR=/tmp/target-chrome`, every cargo command
`--offline`.

The window is now undecorated on all three platforms
(`ViewportBuilder::with_decorations(false)`) and draws its own title strip: 32 px
across the top, in the app's palette, with the three macOS dots at the left. The
strip is also the window's drag handle and the frame of the eight resize bands.
The app therefore looks the same on macOS, Windows and Linux — no traffic lights
from AppKit, no caption from Windows, no header bar from GTK.

## What the strip is

| | Value | Where |
| --- | --- | --- |
| Strip | 32 px tall, the full width, at y = 0 | `chrome::STRIP_HEIGHT`, `strip_rect` |
| Fill | `--panel` `#12141e` (measured `(18, 20, 30)`) | `theme::PANEL` |
| Bottom rule | 1 px `--border`, at y = 31 (measured `(32, 34, 43)`) | `theme::BORDER` |
| Dots | three 12 px circles, 8 px apart, the first at x = 20 | `Dot::rect` |
| Dot centres | (26, 16), (46, 16), (66, 16) — centred in the 32 px band | `Dot::centre` |
| Red | `#FF5F57` | `chrome::DOT_RED` |
| Yellow | `#FEBC2E` | `chrome::DOT_YELLOW` |
| Green | `#28C840` | `chrome::DOT_GREEN` |

The three dot colours are the only colours this change adds; the fill and the
hairline are the existing palette (`theme::PANEL`, `theme::BORDER`), which is why
the strip reads as part of the app rather than as a borrowed title bar. Under the
pointer a dot draws a 1 px darker ring inside its own colour; there is no second
colour and no glyph.

`chrome.png` (1180 x 780, in `egui-ui/tests/snapshots/`) is the picture: rows 0
to 31 are the band, row 31 is the rule, row 32 is `--base` `(0, 0, 0)`.

## What each gesture asks for

The decision is a pure function; drawing and sending are separate, so the tests
can check one without a window (`egui-ui/src/chrome.rs`).

| Gesture | `Action` | `egui::ViewportCommand` |
| --- | --- | --- |
| Red dot | `Close` | `Close` — the close the platform's button asked for, so `FioApp::on_close_requested` still runs, still asks before cancelling running transfers, and `Shell::shutdown` still runs before the window goes |
| Yellow dot | `Minimize` | `Minimized(true)` |
| Green dot | `ToggleMaximize` | `Maximized(!maximized)` — maximise a normal window, restore a maximised one |
| Drag on the strip | `Drag` | `StartDrag` |
| Double click on the strip | `ToggleMaximize` | `Maximized(!maximized)` |
| Press on an edge or corner | `Resize(dir)` | `BeginResize(dir)` |
| Cmd/Ctrl+W, Cmd/Ctrl+Q | `Close` | `Close` |

`egui` 0.33.3's command is `ViewportCommand::Minimized(bool)`; there is no
`Minimize(true)` in this version.

The dots take precedence over the band: each dot is its own 12 px widget added
after the band (so it is on top), and the band's own decision
(`chrome::press_action`) refuses any press whose origin is inside a dot. A click
below y = 32 is nobody's — it reaches the app, which is why the strip does not
swallow the screens' clicks.

The strip is a `TopBottomPanel`, so it is laid out before the sidebar and the
central panel: the app's layout starts at y = 32, the window is still
1180 x 780 overall (the strip is inside that, not added to it), and `--fixture`
draws exactly what the running app draws.

Cmd/Ctrl+W and Cmd/Ctrl+Q are new in this branch — the app bound no shortcuts
before, and on Linux and Windows a decorated window carried them in its system
menu. They now go through `chrome::shortcut` and send the same `Close`.

## Resizing without decorations

Eight 6 px bands (four corners first, then the four edges, so they never overlap)
sit in `Order::Foreground` areas, show `CursorIcon::ResizeNwSe` and friends under
the pointer, and send `ViewportCommand::BeginResize(dir)` with the matching
`egui::viewport::ResizeDirection`. `MIN_WIDTH` / `MIN_HEIGHT` are still the
window's `with_min_inner_size`, so the platform enforces the floor.

**`BeginResize` does nothing on macOS.** winit 0.30.13 implements `drag_window`
(`window.performWindowDragWithEvent`) and `set_maximized` (`window.zoom`) but
`drag_resize_window` returns `Err(ExternalError::NotSupported)`
(`winit-0.30.13/src/platform_impl/macos/window_delegate.rs:1179`), and a
borderless macOS window has no edge to grab either. A patch that stopped at
`BeginResize` would leave the macOS window unresizable. So on macOS the same drag
is also turned into `ViewportCommand::InnerSize` (plus `OuterPosition` for the
west and north edges) from `chrome::resize_drag`, which clamps to
`MIN_WIDTH`/`MIN_HEIGHT` and keeps the opposite edge still. That is egui's own
API — no platform call, no `unsafe`, nothing added to `sys/`. `NATIVE_DRAG_RESIZE`
is a `cfg!(target_os = "macos")`, not a call into the target, and Windows and
X11/Wayland take the native path alone.

Not verified by running the app: this session has no display. The commands are
the documented ones and the vendored winit/egui sources above are what they are
measured against.

## Files

| File | Change |
| --- | --- |
| `egui-ui/src/chrome.rs` | new: the strip, the dots, the eight resize bands, and the pure seam (`Dot`, `Action`, `command`, `press_action`, `strip_rect`, `dot_at`, `resize_rects`, `resize_at`, `resize_cursor`, `resize_drag`, `shortcut`) with 16 tests |
| `egui-ui/src/main.rs` | `.with_decorations(false)`; the title stays for the dock, the taskbar and Mission Control |
| `egui-ui/src/app.rs` | `chrome::show(ctx)` is the first thing `FioApp::draw` does |
| `egui-ui/src/lib.rs` | `pub mod chrome` |
| `egui-ui/src/views/{sidebar,watch,settings,space}.rs` | the test harnesses draw the same chrome, so the goldens are pictures of a window that has a strip |
| `egui-ui/tests/snapshots.rs` | five new tests: the strip's geometry and pixels, the dots' commands, the drag, the strip's precedence, and the undecorated builder; writes `chrome.png` |
| `egui-ui/tests/snapshots/*.png` | six re-blessed, two regenerated, one new (below) |

No new dependency. No `unsafe` (`#![forbid(unsafe_code)]` still holds). Nothing
in `core/`, `sys/`, transfers, Space or Portal changed.

## The pictures

Every golden is the same renderer at the same size as before; all of them now
show the strip. Measured against the previous PNGs, row by row:

| Golden | Size | Below the strip |
| --- | --- | --- |
| `chrome.png` | 1180 x 780 | **new** — the whole Transfers window |
| `space.png` | 1180 x 780 | 3.57 % of pixels differ from the old picture shifted down 32 px: the sidebar's bottom-anchored blocks move up 32 px (the panel is 32 px shorter) and the Space chart is drawn into the shorter panel |
| `space-empty.png` | 1180 x 780 | 0.63 % differ, all in the sidebar's columns |
| `watch_portal.png` | 956 x 780 | **0 pixels** differ from the old picture shifted down 32 px |
| `watch_duplex.png` | 956 x 780 | **0 pixels** differ |
| `settings_portal.png` | 956 x 2400 | **3 pixels** differ |
| `sidebar_224x780_signed_in.png` | 224 x 780 | 3.60 % differ: the account row is pinned to the panel's bottom, so it moves up 32 px |
| `sidebar_224x780_signed_out.png` | 224 x 780 | 6.26 % differ, the same reason |
| `watch_editor.png` | 956 x 780 | 18.30 % differ: the modal is centred in the screen, so it re-centres 16 px higher |

`space.png` and `space-empty.png` are regenerated from the app's own render
(`/tmp/egui-shots/`, written by `tests/snapshots.rs`) the way they were made
before; the other six are re-blessed with `UPDATE_SNAPSHOTS=1 cargo test
--offline -p fiotransfer-egui`.

**The framing is measured, not seen.** This session has no vision: every number
above comes from reading the PNGs back — the dot colours at their centres, the
strip's fill and hairline rows, the accesskit rects of the three dots (12 x 12 at
x = 20, 40, 60, y = 10), and the per-pixel comparison against the previous
goldens.

## Tests

`cargo test --offline --workspace --no-fail-fast`: **681 passed, 2 failed, 1
ignored**. Both failures are the same pre-existing version constant, not this
branch:

| Test | Assertion | State on `origin/main` |
| --- | --- | --- |
| `tests/heartbeat.rs:394` | `app_version == "1.6.1"`, the crate is `1.7.0` | fails identically (checked in a worktree at `origin/main`) |
| `core/src/portal/heartbeat.rs:399` | same constant, same message | same file, untouched by this branch |

`-p fiotransfer-egui`, which is the whole of this change: **268 passed, 0
failed** (240 lib, 6 bin, 8 `prop_space`, 14 `snapshots`). The 16 new tests in
`chrome.rs` cover the dot geometry, the colours, the three dot actions and the
restore, the press precedence (strip drags, dots do not, below the strip is
nobody's), the eight bands and their non-overlap, the drag maths and its floor at
`MIN_WIDTH`/`MIN_HEIGHT`, the cursors, the platform switch, Cmd/Ctrl+W and
Cmd/Ctrl+Q (and a bare W or Q doing nothing), and a real `egui` pass where a
click on each dot asks for that dot's action. The five new tests in
`tests/snapshots.rs` are listed in the Files table.
