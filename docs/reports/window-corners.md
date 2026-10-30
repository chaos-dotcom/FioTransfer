# The window's round corners, and no seam under the strip

Branch `corners`, cut from `origin/main` at `a4bc704` ("version 1.8.0"). Worktree
`/tmp/corners`, `CARGO_TARGET_DIR=/tmp/target-corners`, every cargo command
`--offline`.

Two regressions came with the custom window chrome (`#47`): the window lost the
~10 px rounded corners macOS used to draw, and the title strip no longer met the
page cleanly — there was a visible line and a step where the strip ended. Both
are fixed here, on all platforms, without giving up the strip: the three dots
are the app's, the strip is still the window's drag handle and its resize frame,
and nothing platform-specific was added.

**Everything below is measured from the PNGs, not seen.** This session has no
vision: every number is read back out of the rendered pixels.

## 1. The corners: the app draws its own shape

`main.rs` asks for an undecorated window (`with_decorations(false)`), and an
undecorated window is a square rectangle: no platform rounds it any more. Neither
of the two routes the brief lists was needed in `sys/`; the first one worked.

| | |
| --- | --- |
| `main.rs` | `.with_transparent(true)` — the window is transparent, so a pixel the app does not paint is *unpainted*, not black |
| `app.rs` | `clear_color` is `[0, 0, 0, 0]` instead of `--base`, so the four corner regions stay clear |
| `chrome.rs` | `WINDOW_RADIUS_PX = 10`, and one corner radius per window corner: `CORNER_NW`, `CORNER_NE`, `CORNER_SW`, `CORNER_SE` |
| the four surfaces | each paints the corner it owns: the strip's sidebar column `CORNER_NW` (`--panel`), the strip's page column `CORNER_NE` (`--base`), the sidebar `CORNER_SW` (`--panel`), the central panel `CORNER_SE` (`--base`) |

The window shape is therefore the union of four rounded fills that already tile
the window; there is no fifth "window background" rect, and no `unsafe` anywhere
(`egui-ui` is still `#![forbid(unsafe_code)]`, and `sys/` was not touched).

Measured on `chrome.png` (1180 x 780), per corner, in the 10 x 10 box at that
corner:

| | opaque | partly transparent | fully transparent |
| --- | --- | --- | --- |
| all four corners **before** | 100 | 0 | **0** |
| all four corners **after** | 69 | 14 | **17** |

Before, `(0, 0)` read `(18, 20, 30, 255)` — square, painted. After, it reads
`(0, 0, 0, 0)`, and 17 pixels of each corner box are never painted at all, with
14 more anti-aliased on the arc: a quarter circle of radius 10, the same at all
four corners. Inside the arc the four surfaces are unchanged: `--panel` in the two
left corners, `--base` in the two right ones.

**The shadow is kept.** `with_transparent(true)` makes the window non-opaque
(`setOpaque(false)` plus a clear background colour — `winit-0.30.13`,
`src/platform_impl/macos/window_delegate.rs:870`). It does not touch
`hasShadow`: winit only calls `setHasShadow(false)` when the platform asks for
it, which eframe never does, so the window keeps the shadow it always had.
Whether AppKit shapes that shadow to the rounded alpha or to the rectangle is the
one thing here that **cannot be measured without a window**: nothing was opened in
this session. Expect the rounded shape, and look at the Mac.

The radius is a constant in `chrome.rs`, not a platform call, so the drawing code
is the same on macOS, Windows and Linux and the goldens are the same picture
everywhere. Where the platform cannot composite a transparent window (an X11
session with no compositor), the corners would show the window server's default
background instead of the desktop: unverified here, and the one thing to look at
on a Linux box.

### What it took to make the corner survive a scrolled screen

Two fills were quietly squaring the bottom corners off, and both are gone:

* `theme::main_frame()` (the `.view` frame inside the scroll area) filled
  `--base`; a `Frame`'s rect follows its *content*, so a screen taller than the
  window painted a rect that hung past the window's bottom edge, square. The page
  is already `--base` from the panel, so the fill is now `Frame::NONE`. Measured:
  with 12 watch rows (scrolling) the bottom-right corner was 0 % transparent
  before this and is the same quarter circle after.
* the central panel's own fill moved from its `Frame` to an explicit
  `rect_filled(ui.max_rect(), CORNER_SE, --base)`: the panel's rect is the
  window's rect whatever the screen's length, a frame's rect is not.

## 2. The seam: the strip is the window, not a band on it

The seam was both things the brief suspected, and both are gone.

The strip used to be one `--panel` band across the full width, with a 1 px
`--border` rule along its bottom edge. Over the page that reads as a line *and* a
step; over the sidebar only the line shows, because the sidebar is `--panel` too:

| x | row 30 | row 31 (the rule) | row 32 (the page) |
| --- | --- | --- | --- |
| 600 (page) **before** | `(18, 20, 30)` | `(32, 34, 43)` | `(0, 0, 0)` |
| 600 (page) **after** | `(0, 0, 0)` | `(0, 0, 0)` | `(0, 0, 0)` |
| 100 (sidebar) **before** | `(18, 20, 30)` | `(32, 34, 43)` | `(18, 20, 30)` |
| 100 (sidebar) **after** | `(18, 20, 30)` | `(18, 20, 30)` | `(18, 20, 30)` |

Row 31 was `(32, 34, 43)` — `--border` `#ffffff0f` over `--panel` — across 1179 of
the window's 1180 columns. The 1180th, x = 223, is the sidebar's own right edge,
which was already `(32, 34, 43)` and still is.

The fix is not "make the strip the page colour": that would have moved the same
step to the sidebar's 224 px. The strip paints no colour of its own any more —
each of its columns paints the surface it continues:

* `--base` over the page's column (the window's top-right corner rounded),
* `--panel` over the sidebar's column (the window's top-left corner rounded),
* the sidebar's 1 px `--border` right edge carried up from y = 32 to y = 0, so the
  divider runs the window's whole height instead of stopping 32 px short,
* no bottom rule. Nothing is drawn between the strip and the window below it.

`chrome::strip_columns` is the seam: the split is `.app { grid-template-columns:
224px 1fr }` (`theme::size::SIDEBAR_W`), and the two columns are the whole strip,
side by side, with no gap.

## What moved in the goldens

Every golden is the same renderer at the same size as before. Re-blessed with
`UPDATE_SNAPSHOTS=1 cargo test --offline -p fiotransfer-egui`; the numbers are a
per-pixel comparison against the previous PNG.

| Golden | Size | Pixels that differ | Where |
| --- | --- | --- | --- |
| `chrome.png` | 1180 x 780 | 30,939 (3.36 %) | rows 0–31 (the strip's page column, and row 31's rule: 1179 px), rows 770–779 (the two bottom corners) |
| `watch_portal.png` | 956 x 780 | 23,709 (3.18 %) | rows 0–31 only |
| `watch_duplex.png` | 956 x 780 | 23,709 (3.18 %) | rows 0–31 only |
| `watch_editor.png` | 956 x 780 | 23,709 (3.18 %) | rows 0–31 only |
| `settings_portal.png` | 956 x 2400 | 23,709 (1.03 %) | rows 0–31 only |
| `sidebar_224x780_signed_in.png` | 224 x 780 | 316 (0.18 %) | rows 0–31 (the divider carried up, the top-left corner), rows 770–779 (the bottom-left corner) |
| `sidebar_224x780_signed_out.png` | 224 x 780 | 316 (0.18 %) | the same |

`space.png` and `space-empty.png` are written to `/tmp/egui-shots` and not
compared; no content area moved, so the screens below y = 32 are pixel-identical
in every golden above (the watch and settings pictures do not change at all
below row 31). The four watch/settings pictures have no bottom-corner change
because those harnesses draw a bare `CentralPanel` rather than the app's
`FioApp::draw`, which is where `CORNER_SE` is painted.

## Tests

Two new pixel tests, plus two new pure tests in `chrome.rs`:

| Test | What it pins |
| --- | --- |
| `snapshots.rs::the_strip_meets_the_page_without_a_line_or_a_step` | for all 1180 columns: row 31 = row 32; and, clear of the corner rounding, row 0 = row 31. Plus `--panel` over the sidebar's column and `--base` over the page's, in the strip and below it |
| `snapshots.rs::the_window_corners_are_round_and_unpainted` | all four corner pixels have alpha 0, the first 5 px along both edges of each corner are unpainted, at least 10 of each 10 x 10 corner box is unpainted, and a radius in the window is the surface that owns that corner |
| `chrome.rs::the_strip_paints_the_two_surfaces_it_continues` | the two columns are 224 px and the rest, side by side, no gap, together the whole strip; a strip narrower than the sidebar has no negative-width column |
| `chrome.rs::each_window_corner_is_rounded_and_only_that_corner` | 10 px, one corner per constant, nothing on the other three |

`cargo test --offline --workspace --no-fail-fast`: **all green, 0 failed** (the
`1 ignored` is the pre-existing one). `cargo test --offline -p fiotransfer-egui`:
240 lib tests, 16 `snapshots`, 8 `prop_space`, 6 bin — all pass. No new
dependency, no `unsafe`, nothing in `core/`, `sys/`, transfers or Space changed.

## Files

| File | Change |
| --- | --- |
| `egui-ui/src/main.rs` | `.with_transparent(true)`, and why |
| `egui-ui/src/app.rs` | `clear_color` → transparent; the page painted over the panel's rect with `CORNER_SE` |
| `egui-ui/src/chrome.rs` | `WINDOW_RADIUS_PX`, the four corner constants, `strip_columns`, the two-column strip, the divider carried to y = 0, the bottom rule gone |
| `egui-ui/src/theme.rs` | `main_frame()` no longer fills (the panel does) |
| `egui-ui/src/views/sidebar.rs` | the sidebar's fill rounds `CORNER_SW` |
| `egui-ui/tests/snapshots.rs` | two new pixel tests; the chrome test's two strip-colour assertions follow the new fill |
| `egui-ui/tests/snapshots/*.png` | seven re-blessed (above) |
