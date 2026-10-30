//! The window's own chrome: the title strip, its three dots and the resize
//! edges — drawn by the app, so the window looks the same on macOS, Windows and
//! Linux.
//!
//! `main.rs` asks for an undecorated window
//! (`ViewportBuilder::with_decorations(false)`), so no platform draws a title
//! bar: no macOS traffic lights, no Windows caption, no GTK header bar. What a
//! window's chrome still has to do — be moved, closed, minimised, maximised and
//! resized — is [`show`]'s:
//!
//! * a 32 px strip across the top of the window, in the app's own palette, with
//!   the three 12 px dots at its left (the only colours this module adds:
//!   `#FF5F57`, `#FEBC2E` and `#28C840`);
//! * eight 6 px hit bands along the window's edges and corners.
//!
//! ```text
//!  0                   20  26  46  66                                 1180
//!  +----------------------------------------------------------------------+
//!  |   o   o   o                                                          |  32
//!  +----------------------------------------------------------------------+
//!  |  sidebar, 224 px  |  the screen                                      |
//! ```
//!
//! The dots are 12 px circles, 8 px apart, the first starting at x = 20 and all
//! centred in the strip: their centres are (26, 16), (46, 16) and (66, 16).
//!
//! ## The strip has no colour of its own, so there is no seam under it
//!
//! The strip paints no colour of its own. Its two columns paint the surface they
//! continue — `--panel` over the sidebar's 224 px, `--base` over the page — so
//! every column of the window has one colour from row 0 to the last row, and
//! the boundary between the strip and the window below it is not a line, a step
//! or a gradient: it is not there. The 1 px `--border` rule that used to sit on
//! the strip's bottom edge went with it; the sidebar's own `--border` right edge
//! is carried up to y = 0, so the divider runs the window's whole height.
//!
//! ## Rounded window corners
//!
//! A decorated macOS window is rounded; an undecorated one is a square
//! rectangle, which is what `with_decorations(false)` left behind. The app draws
//! the corners itself: `main.rs` asks for a transparent window
//! (`with_transparent(true)`), and every surface that reaches a window corner
//! fills a rounded rect there — the strip's two columns carry the top corners
//! ([`CORNER_NW`], [`CORNER_NE`]), the sidebar the bottom-left ([`CORNER_SW`])
//! and the central panel the bottom-right ([`CORNER_SE`]), all with
//! [`WINDOW_RADIUS_PX`]. The pixels outside a corner are never painted, so the
//! window is transparent there and the desktop shows through — the shape the
//! window server rounds and shadows is the app's own.
//!
//! The radius is a constant here rather than a platform value: it is the same 10
//! px on all three targets, so the goldens in `tests/snapshots.rs` are the same
//! picture everywhere.
//!
//! ## Why the seam looks like this
//!
//! Everything that decides *what* the window should do is a plain function with
//! no `egui::Context` in it, so the tests at the bottom of the file can check it
//! without a window: [`press_action`] (what a press at a point asks for),
//! [`command`] (the [`egui::ViewportCommand`] an action becomes), [`resize_at`]
//! (which edge a point is on), [`resize_drag`] (the size and position a resize
//! drag asks for) and [`shortcut`] (Cmd/Ctrl+W, Cmd/Ctrl+Q). Drawing and sending
//! live in [`strip`], [`resize_edges`] and [`show`], which do nothing else.
//!
//! ## Resizing an undecorated window
//!
//! egui's own answer is [`egui::ViewportCommand::BeginResize`], and that is what
//! this module sends on every platform. On Windows and on the X11/Wayland
//! backends winit turns it into a native resize drag. On macOS it does not:
//! winit 0.30.13's `drag_resize_window` is `Err(ExternalError::NotSupported)`
//! (`src/platform_impl/macos/window_delegate.rs:1179`), and the same file shows
//! `drag_window` and `set_maximized` *are* implemented, so only the resize is
//! missing. Since a borderless macOS window has no edge to grab either, an app
//! that stopped at `BeginResize` could not be resized on macOS at all.
//!
//! So on macOS the same drag is also turned into an
//! [`egui::ViewportCommand::InnerSize`] (plus `OuterPosition` when the drag is
//! on the west or north edge) from [`resize_drag`]: egui's own commands, no
//! platform call and no `unsafe`, which is why this stays in `egui-ui` rather
//! than `sys/`. [`NATIVE_DRAG_RESIZE`] is the one platform switch, and it is a
//! `cfg!` on the target, not a call into it.

use egui::{
    Color32, CornerRadius, CursorIcon, Frame, Id, Key, KeyboardShortcut, Modifiers, Pos2, Rect,
    Sense, Stroke, Vec2, ViewportCommand, pos2, vec2, viewport::ResizeDirection,
};

use crate::theme;
use crate::{MIN_HEIGHT, MIN_WIDTH};

// ---------------------------------------------------------------------------
// The strip's geometry
// ---------------------------------------------------------------------------

/// The strip's height: 32 px, the full width of the window.
pub const STRIP_HEIGHT: f32 = 32.0;
/// A dot's diameter, 12 px.
pub const DOT_SIZE: f32 = 12.0;
/// The gap between two dots, edge to edge: 8 px.
pub const DOT_GAP: f32 = 8.0;
/// The first dot's left edge, from the window's left edge: 20 px.
pub const DOT_FIRST_X: f32 = 20.0;
/// The hit band along each edge and corner: 6 px.
pub const RESIZE_BAND: f32 = 6.0;

/// The window's own corner radius: 10 px, what a decorated macOS window had
/// before `with_decorations(false)` took the platform's corners away. It is the
/// app's number now — see the module note on the rounded window corners — so it
/// is a constant here rather than a platform default, and it is the same on
/// every OS the app builds for, which keeps `tests/snapshots.rs` portable.
pub const WINDOW_RADIUS_PX: u8 = 10;

/// 10 px on the window's north-west corner — the sidebar's column at the
/// window's top-left.
pub const CORNER_NW: CornerRadius = CornerRadius {
    nw: WINDOW_RADIUS_PX,
    ne: 0,
    sw: 0,
    se: 0,
};
/// 10 px on the window's north-east corner — the page at the window's top-right.
pub const CORNER_NE: CornerRadius = CornerRadius {
    nw: 0,
    ne: WINDOW_RADIUS_PX,
    sw: 0,
    se: 0,
};
/// 10 px on the window's south-west corner — the sidebar's column at the
/// window's bottom-left.
pub const CORNER_SW: CornerRadius = CornerRadius {
    nw: 0,
    ne: 0,
    sw: WINDOW_RADIUS_PX,
    se: 0,
};
/// 10 px on the window's south-east corner — the page at the window's
/// bottom-right.
pub const CORNER_SE: CornerRadius = CornerRadius {
    nw: 0,
    ne: 0,
    sw: 0,
    se: WINDOW_RADIUS_PX,
};

/// `#FF5F57` — the red dot.
pub const DOT_RED: Color32 = Color32::from_rgb(0xff, 0x5f, 0x57);
/// `#FEBC2E` — the yellow dot.
pub const DOT_YELLOW: Color32 = Color32::from_rgb(0xfe, 0xbc, 0x2e);
/// `#28C840` — the green dot.
pub const DOT_GREEN: Color32 = Color32::from_rgb(0x28, 0xc8, 0x40);

/// Whether this platform's native resize drag works. See the module note:
/// winit cannot drag-resize an undecorated window on macOS, so there the drag is
/// applied with `InnerSize`/`OuterPosition` instead.
pub const NATIVE_DRAG_RESIZE: bool = !cfg!(target_os = "macos");

// ---------------------------------------------------------------------------
// The dots
// ---------------------------------------------------------------------------

/// One of the three dots at the strip's left.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum Dot {
    Red,
    Yellow,
    Green,
}

impl Dot {
    /// The three, left to right — the order they are drawn and hit-tested in.
    pub const ALL: [Self; 3] = [Self::Red, Self::Yellow, Self::Green];

    /// The dot's colour: the three the brief names, and the only colours this
    /// module adds to the palette.
    pub fn color(self) -> Color32 {
        match self {
            Self::Red => DOT_RED,
            Self::Yellow => DOT_YELLOW,
            Self::Green => DOT_GREEN,
        }
    }

    /// The dot's name, which is also its accessibility label: a screen reader
    /// user gets the three window buttons the platform no longer draws.
    pub fn label(self) -> &'static str {
        match self {
            Self::Red => "Close window",
            Self::Yellow => "Minimise window",
            Self::Green => "Maximise window",
        }
    }

    /// The dot's box, in strip-local points: 12 x 12, the first at x = 20, all
    /// vertically centred in the 32 px strip (so 10 px down).
    pub fn rect(self) -> Rect {
        let index = match self {
            Self::Red => 0.0,
            Self::Yellow => 1.0,
            Self::Green => 2.0,
        };
        let left = DOT_FIRST_X + index * (DOT_SIZE + DOT_GAP);
        let top = (STRIP_HEIGHT - DOT_SIZE) * 0.5;
        Rect::from_min_size(pos2(left, top), Vec2::splat(DOT_SIZE))
    }

    /// The dot's centre, in strip-local points: (26, 16), (46, 16), (66, 16).
    pub fn centre(self) -> Pos2 {
        self.rect().center()
    }

    /// What pressing this dot asks for.
    pub fn action(self) -> Action {
        match self {
            Self::Red => Action::Close,
            Self::Yellow => Action::Minimize,
            Self::Green => Action::ToggleMaximize,
        }
    }
}

// ---------------------------------------------------------------------------
// Actions and commands
// ---------------------------------------------------------------------------

/// What the chrome asks the window to do: one action per gesture.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Action {
    /// The red dot, and Cmd/Ctrl+W and Cmd/Ctrl+Q: close the window — the same
    /// close the platform's own button asked for, so the shell still runs its
    /// own close path (`FioApp::on_close_requested`).
    Close,
    /// The yellow dot: minimise.
    Minimize,
    /// The green dot, and a double click on the strip: maximise, or restore.
    ToggleMaximize,
    /// A press on the strip itself: let the platform drag the window.
    Drag,
    /// A press on one of the eight edge bands.
    Resize(ResizeDirection),
}

/// The command an [`Action`] is.
///
/// `maximized` is the window's state at the start of the frame: the green dot
/// and a double click ask for the opposite of it, so one gesture maximises a
/// normal window and restores a maximised one.
pub fn command(action: Action, maximized: bool) -> ViewportCommand {
    match action {
        Action::Close => ViewportCommand::Close,
        Action::Minimize => ViewportCommand::Minimized(true),
        Action::ToggleMaximize => ViewportCommand::Maximized(!maximized),
        Action::Drag => ViewportCommand::StartDrag,
        Action::Resize(direction) => ViewportCommand::BeginResize(direction),
    }
}

/// The keyboard: the two shortcuts the window has always answered, now that no
/// menu or title bar carries them. `Modifiers::COMMAND` is Cmd on macOS and Ctrl
/// elsewhere, so these are exactly Cmd+W / Ctrl+W and Cmd+Q / Ctrl+Q.
///
/// Consumed from the input queue, so a key press is acted on once. Both close
/// the window, which is what the app has always done: `Cmd+Q` is the platform's
/// "quit this window", and closing runs the shell's own shutdown.
pub fn shortcut(ctx: &egui::Context) -> Option<Action> {
    for key in [Key::W, Key::Q] {
        let shortcut = KeyboardShortcut::new(Modifiers::COMMAND, key);
        if ctx.input_mut(|input| input.consume_shortcut(&shortcut)) {
            return Some(Action::Close);
        }
    }
    None
}

// ---------------------------------------------------------------------------
// Geometry: the strip, the dots and the edges
// ---------------------------------------------------------------------------

/// The strip, in window coordinates: the top 32 px, the full width.
pub fn strip_rect(window: Rect) -> Rect {
    Rect::from_min_size(window.min, vec2(window.width(), STRIP_HEIGHT))
}

/// The dot under a point, in window coordinates. The dots are the first thing
/// the strip hit-tests, so a press on one is a press on the button and never a
/// drag.
pub fn dot_at(window: Rect, pos: Pos2) -> Option<Dot> {
    let local = pos - window.min.to_vec2();
    Dot::ALL.into_iter().find(|dot| dot.rect().contains(local))
}

/// What a press at `pos` (window coordinates) asks for.
///
/// * a dot: that dot's action — the dots take precedence over the strip;
/// * the strip, double clicked: [`Action::ToggleMaximize`];
/// * the strip: [`Action::Drag`], which is the platform's own window drag;
/// * anywhere else: nothing, so a click below the strip reaches the app.
pub fn press_action(window: Rect, pos: Pos2, double_clicked: bool) -> Option<Action> {
    if let Some(dot) = dot_at(window, pos) {
        return Some(dot.action());
    }
    if !strip_rect(window).contains(pos) {
        return None;
    }
    Some(if double_clicked {
        Action::ToggleMaximize
    } else {
        Action::Drag
    })
}

/// The eight resize bands, in window coordinates, corners first so the bands
/// never overlap: the edge bands stop where the corners begin.
pub fn resize_rects(window: Rect) -> [(ResizeDirection, Rect); 8] {
    let band = RESIZE_BAND
        .min(window.width() * 0.5)
        .min(window.height() * 0.5)
        .max(0.0);
    let (left, right) = (window.left(), window.right());
    let (top, bottom) = (window.top(), window.bottom());
    let corner = |x: f32, y: f32| Rect::from_min_max(pos2(x, y), pos2(x + band, y + band));
    [
        (ResizeDirection::NorthWest, corner(left, top)),
        (ResizeDirection::NorthEast, corner(right - band, top)),
        (ResizeDirection::SouthWest, corner(left, bottom - band)),
        (
            ResizeDirection::SouthEast,
            corner(right - band, bottom - band),
        ),
        (
            ResizeDirection::North,
            Rect::from_min_max(pos2(left + band, top), pos2(right - band, top + band)),
        ),
        (
            ResizeDirection::South,
            Rect::from_min_max(pos2(left + band, bottom - band), pos2(right - band, bottom)),
        ),
        (
            ResizeDirection::West,
            Rect::from_min_max(pos2(left, top + band), pos2(left + band, bottom - band)),
        ),
        (
            ResizeDirection::East,
            Rect::from_min_max(pos2(right - band, top + band), pos2(right, bottom - band)),
        ),
    ]
}

/// Which edge a point (window coordinates) is on, if any.
pub fn resize_at(window: Rect, pos: Pos2) -> Option<ResizeDirection> {
    resize_rects(window)
        .into_iter()
        .find(|(_, rect)| rect.contains(pos))
        .map(|(direction, _)| direction)
}

/// The pointer a resize band shows: the arrow that points along the edge the
/// drag would move.
pub fn resize_cursor(direction: ResizeDirection) -> CursorIcon {
    match direction {
        ResizeDirection::North | ResizeDirection::South => CursorIcon::ResizeVertical,
        ResizeDirection::East | ResizeDirection::West => CursorIcon::ResizeHorizontal,
        // `ResizeNwSe` is the arrow between north-west and south-east.
        ResizeDirection::NorthWest | ResizeDirection::SouthEast => CursorIcon::ResizeNwSe,
        ResizeDirection::NorthEast | ResizeDirection::SouthWest => CursorIcon::ResizeNeSw,
    }
}

/// The size and position a resize drag asks for.
///
/// `inner` is the window's content size and `outer` its position when the drag
/// began, and `delta` is how far the pointer has moved since. A drag on the east
/// or south edge only changes the size; a drag on the west or north edge moves
/// the window as well, so the edge opposite the pointer stays where it is. The
/// size never goes under [`MIN_WIDTH`] / [`MIN_HEIGHT`], and when the clamp
/// bites the position follows the clamped size, so the edge stays under the
/// pointer.
pub fn resize_drag(
    direction: ResizeDirection,
    inner: Vec2,
    outer: Pos2,
    delta: Vec2,
) -> (Vec2, Pos2) {
    let west = matches!(
        direction,
        ResizeDirection::West | ResizeDirection::NorthWest | ResizeDirection::SouthWest
    );
    let east = matches!(
        direction,
        ResizeDirection::East | ResizeDirection::NorthEast | ResizeDirection::SouthEast
    );
    let north = matches!(
        direction,
        ResizeDirection::North | ResizeDirection::NorthWest | ResizeDirection::NorthEast
    );
    let south = matches!(
        direction,
        ResizeDirection::South | ResizeDirection::SouthWest | ResizeDirection::SouthEast
    );

    let mut size = inner;
    if east {
        size.x = (inner.x + delta.x).max(MIN_WIDTH);
    } else if west {
        size.x = (inner.x - delta.x).max(MIN_WIDTH);
    }
    if south {
        size.y = (inner.y + delta.y).max(MIN_HEIGHT);
    } else if north {
        size.y = (inner.y - delta.y).max(MIN_HEIGHT);
    }

    let mut pos = outer;
    if west {
        pos.x = outer.x + (inner.x - size.x);
    }
    if north {
        pos.y = outer.y + (inner.y - size.y);
    }
    (size, pos)
}

/// The geometry a resize drag began from, kept between frames on macOS only.
/// (`Default` is what `IdTypeMap::remove_temp` wants; the value it hands back is
/// thrown away.)
#[derive(Clone, Copy, Debug)]
struct DragStart {
    direction: ResizeDirection,
    inner: Vec2,
    outer: Pos2,
    origin: Pos2,
}

impl Default for DragStart {
    fn default() -> Self {
        Self {
            direction: ResizeDirection::North,
            inner: Vec2::ZERO,
            outer: Pos2::ZERO,
            origin: Pos2::ZERO,
        }
    }
}

/// The window's content size and its position, as egui last reported them.
fn window_geometry(ctx: &egui::Context) -> Option<(Vec2, Pos2)> {
    ctx.input(|input| {
        let viewport = input.viewport();
        Some((viewport.inner_rect?.size(), viewport.outer_rect?.min))
    })
}

// ---------------------------------------------------------------------------
// Drawing
// ---------------------------------------------------------------------------

/// The strip's two columns, in window coordinates: the sidebar's own column
/// (which carries the window's north-west corner) and the page's (which carries
/// the north-east one). The split is `.app { grid-template-columns: 224px 1fr }`
/// — [`theme::size::SIDEBAR_W`].
///
/// The strip is not a band of its own colour laid over the window: each column
/// paints the surface it continues, so the strip and the window below it are one
/// picture. See the module note on why there is no seam under the strip.
pub fn strip_columns(strip: Rect) -> (Rect, Rect) {
    let split = strip.left() + theme::size::SIDEBAR_W.min(strip.width());
    (
        Rect::from_min_max(strip.min, pos2(split, strip.bottom())),
        Rect::from_min_max(pos2(split, strip.top()), strip.max),
    )
}

/// The title strip: 32 px across the top, the three dots at its left. Returns
/// what the strip asked for on this frame, if anything.
///
/// The panel is where the layout starts: a `TopBottomPanel` takes its height off
/// the top of the window, so the sidebar and the screen below it begin at y = 32
/// and nothing is clipped. The band is a single `Sense::click_and_drag` widget
/// over the whole strip; the three dots are added after it, so a press on a dot
/// belongs to the dot (the later widget is the one on top) and the band never
/// sees it as a drag.
pub fn strip(ctx: &egui::Context) -> Option<Action> {
    let mut action = None;
    // No fill on the frame: the strip paints its own two columns below. A single
    // `--panel` band here would be a colour of its own, and that is the seam this
    // change removes.
    egui::TopBottomPanel::top(Id::new("fio-chrome-strip"))
        .exact_height(STRIP_HEIGHT)
        .resizable(false)
        .frame(Frame::NONE)
        .show_separator_line(false)
        .show(ctx, |ui| {
            let rect = ui.max_rect();
            let (sidebar_column, page_column) = strip_columns(rect);
            // The page: `--base`, the surface the central panel continues, with
            // the window's north-east corner rounded off.
            ui.painter()
                .rect_filled(page_column, CORNER_NE, theme::BASE);
            // The sidebar's column: `--panel`, the surface the sidebar
            // continues, with the window's north-west corner rounded off. It is
            // painted second so the two meet on the sidebar's own right edge,
            // like every other row of the window.
            ui.painter()
                .rect_filled(sidebar_column, CORNER_NW, theme::PANEL);
            // The sidebar's 1 px `--border` right edge, carried up to the top of
            // the window: without it the divider stops dead 32 px short of the
            // corner and the strip reads as a notch cut into the sidebar.
            ui.painter().vline(
                sidebar_column.right() - theme::size::HAIRLINE * 0.5,
                sidebar_column.y_range(),
                theme::stroke_border(),
            );

            let band = ui.interact(rect, Id::new("fio-chrome-band"), Sense::click_and_drag());

            for dot in Dot::ALL {
                let dot_rect = dot.rect().translate(rect.min.to_vec2());
                let response =
                    ui.interact(dot_rect, Id::new(("fio-chrome-dot", dot)), Sense::click());
                // The accessibility node: the three window buttons, by name.
                response.widget_info(|| {
                    egui::WidgetInfo::labeled(
                        egui::WidgetType::Button,
                        ui.is_enabled(),
                        dot.label(),
                    )
                });
                paint_dot(ui, dot, dot_rect, response.hovered());
                if response.clicked() {
                    action = Some(dot.action());
                }
            }

            // The strip itself. `press_action` is the whole decision: a press
            // that began on a dot is the dot's, so the band's drag is ignored
            // there and the dot's own `clicked()` above answers it.
            let origin = ctx.input(|input| input.pointer.press_origin());
            if let Some(origin) = origin {
                let double = band.double_clicked();
                if (double || band.drag_started()) && dot_at(rect, origin).is_none() {
                    action = press_action(rect, origin, double);
                }
            }
        });
    action
}

/// One dot: a 12 px circle in its own colour, with a darker ring under the
/// pointer.
fn paint_dot(ui: &egui::Ui, dot: Dot, rect: Rect, hovered: bool) {
    let radius = DOT_SIZE * 0.5;
    ui.painter()
        .circle_filled(rect.center(), radius, dot.color());
    if hovered {
        // A slightly darker ring, so the pointer shows which button it is over.
        ui.painter().circle_stroke(
            rect.center(),
            radius - 0.5,
            Stroke::new(1.0_f32, Color32::from_rgba_unmultiplied(0, 0, 0, 90)),
        );
    }
}

/// The eight resize bands: invisible, 6 px wide, along the window's edges and
/// corners. Each shows the resize cursor and starts a resize drag.
pub fn resize_edges(ctx: &egui::Context) {
    let window = ctx.content_rect();
    let start = Id::new("fio-chrome-resize-start");
    for (direction, rect) in resize_rects(window) {
        if rect.width() <= 0.0 || rect.height() <= 0.0 {
            continue;
        }
        egui::Area::new(Id::new(("fio-chrome-edge", edge_name(direction))))
            .fixed_pos(rect.min)
            .order(egui::Order::Foreground)
            .interactable(true)
            .show(ctx, |ui| {
                let response = ui.allocate_response(rect.size(), Sense::drag());
                if response.hovered() || response.dragged() {
                    ui.ctx().set_cursor_icon(resize_cursor(direction));
                }
                if response.drag_started() {
                    // The native drag: the edge follows the pointer until the
                    // button is released. Windows and X11/Wayland take this.
                    ui.ctx()
                        .send_viewport_cmd(command(Action::Resize(direction), false));
                    // macOS: winit cannot drag-resize (see the module note), so
                    // remember the geometry this drag began from.
                    if !NATIVE_DRAG_RESIZE {
                        if let Some((inner, outer)) = window_geometry(ui.ctx()) {
                            let origin = ui
                                .ctx()
                                .input(|input| input.pointer.press_origin())
                                .unwrap_or(Pos2::ZERO);
                            ui.ctx().data_mut(|data| {
                                data.insert_temp(
                                    start,
                                    DragStart {
                                        direction,
                                        inner,
                                        outer,
                                        origin,
                                    },
                                );
                            });
                        }
                    }
                }
                if !NATIVE_DRAG_RESIZE && response.dragged() {
                    let from = ui.ctx().data(|data| data.get_temp::<DragStart>(start));
                    let pointer = ui.ctx().input(|input| input.pointer.interact_pos());
                    if let (Some(from), Some(pointer)) = (from, pointer) {
                        let (inner, outer) = resize_drag(
                            from.direction,
                            from.inner,
                            from.outer,
                            pointer - from.origin,
                        );
                        ui.ctx()
                            .send_viewport_cmd(ViewportCommand::InnerSize(inner));
                        ui.ctx()
                            .send_viewport_cmd(ViewportCommand::OuterPosition(outer));
                    }
                }
                if response.drag_stopped() {
                    ui.ctx()
                        .data_mut(|data| data.remove_temp::<DragStart>(start));
                }
            });
    }
}

/// Draw the window's chrome and act on what it asked for.
///
/// The strip is drawn first, before the sidebar and the screen: it takes its
/// 32 px off the top of the window, so the panels below start 32 px lower and
/// the window is still `WINDOW_WIDTH` x `WINDOW_HEIGHT` overall. The resize
/// bands are `Order::Foreground` areas, so they sit above every panel while
/// covering only 6 px of each edge.
pub fn show(ctx: &egui::Context) {
    let maximized = ctx.input(|input| input.viewport().maximized.unwrap_or(false));
    if let Some(action) = strip(ctx).or_else(|| shortcut(ctx)) {
        ctx.send_viewport_cmd(command(action, maximized));
    }
    resize_edges(ctx);
}

/// The Id suffix for an edge: the direction's name, for a stable area Id.
fn edge_name(direction: ResizeDirection) -> &'static str {
    match direction {
        ResizeDirection::North => "n",
        ResizeDirection::South => "s",
        ResizeDirection::East => "e",
        ResizeDirection::West => "w",
        ResizeDirection::NorthEast => "ne",
        ResizeDirection::SouthEast => "se",
        ResizeDirection::NorthWest => "nw",
        ResizeDirection::SouthWest => "sw",
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{WINDOW_HEIGHT, WINDOW_WIDTH};

    fn window() -> Rect {
        Rect::from_min_size(Pos2::ZERO, vec2(WINDOW_WIDTH, WINDOW_HEIGHT))
    }

    // ---------- the strip and its dots ----------

    #[test]
    fn the_strip_is_the_top_32_px_across_the_window() {
        let strip = strip_rect(window());
        assert_eq!(strip.left(), 0.0);
        assert_eq!(strip.right(), WINDOW_WIDTH);
        assert_eq!(strip.top(), 0.0);
        assert_eq!(strip.height(), 32.0);
        assert_eq!(STRIP_HEIGHT, 32.0);
    }

    #[test]
    fn the_strip_paints_the_two_surfaces_it_continues() {
        let strip = strip_rect(window());
        let (sidebar, page) = strip_columns(strip);
        // `.app { grid-template-columns: 224px 1fr }`, carried to the top of the
        // window: the sidebar's column, then the page's.
        assert_eq!(sidebar.min, pos2(0.0, 0.0));
        assert_eq!(sidebar.right(), theme::size::SIDEBAR_W);
        assert_eq!(page.min, pos2(theme::size::SIDEBAR_W, 0.0));
        assert_eq!(page.right(), WINDOW_WIDTH);
        // Side by side, no gap, and together the whole strip.
        assert_eq!(sidebar.right(), page.left());
        assert_eq!(sidebar.union(page), strip);
        assert_eq!(sidebar.height(), STRIP_HEIGHT);
        assert_eq!(page.height(), STRIP_HEIGHT);
        // A strip narrower than the sidebar is all sidebar's column, never a
        // rect with a negative width.
        let narrow = Rect::from_min_size(Pos2::ZERO, vec2(100.0, STRIP_HEIGHT));
        let (only_sidebar, nothing) = strip_columns(narrow);
        assert_eq!(only_sidebar, narrow);
        assert_eq!(nothing.width(), 0.0);
    }

    #[test]
    fn each_window_corner_is_rounded_and_only_that_corner() {
        // 10 px, the radius a decorated macOS window had, on one corner each and
        // nothing on the other three: a surface rounds the corner it holds, so a
        // column that runs down the whole window is not rounded in the middle.
        assert_eq!(WINDOW_RADIUS_PX, 10);
        for (corner, nw, ne, sw, se) in [
            (CORNER_NW, 10, 0, 0, 0),
            (CORNER_NE, 0, 10, 0, 0),
            (CORNER_SW, 0, 0, 10, 0),
            (CORNER_SE, 0, 0, 0, 10),
        ] {
            assert_eq!(
                (corner.nw, corner.ne, corner.sw, corner.se),
                (nw, ne, sw, se),
                "a corner rounds more than the corner it holds"
            );
        }
    }

    #[test]
    fn the_three_dots_are_12_px_circles_8_px_apart_from_x_20() {
        // The brief's numbers, measured on the geometry the strip draws with.
        assert_eq!(DOT_SIZE, 12.0);
        assert_eq!(DOT_GAP, 8.0);
        assert_eq!(DOT_FIRST_X, 20.0);
        assert_eq!(Dot::Red.rect().min, pos2(20.0, 10.0));
        assert_eq!(Dot::Red.rect().size(), vec2(12.0, 12.0));
        assert_eq!(Dot::Yellow.rect().min, pos2(40.0, 10.0));
        assert_eq!(Dot::Green.rect().min, pos2(60.0, 10.0));
        // 8 px between the circles: 20 + 12 + 8 = 40.
        assert_eq!(Dot::Yellow.rect().left() - Dot::Red.rect().right(), DOT_GAP);
        assert_eq!(
            Dot::Green.rect().left() - Dot::Yellow.rect().right(),
            DOT_GAP
        );
        // Vertically centred in the 32 px strip, and all on the same line.
        for dot in Dot::ALL {
            assert_eq!(dot.rect().center().y, STRIP_HEIGHT * 0.5);
            assert!(strip_rect(window()).contains_rect(dot.rect()));
        }
        assert_eq!(Dot::Red.centre(), pos2(26.0, 16.0));
        assert_eq!(Dot::Yellow.centre(), pos2(46.0, 16.0));
        assert_eq!(Dot::Green.centre(), pos2(66.0, 16.0));
    }

    #[test]
    fn the_dots_are_the_three_macos_colours() {
        assert_eq!(Dot::Red.color(), Color32::from_rgb(0xff, 0x5f, 0x57));
        assert_eq!(Dot::Yellow.color(), Color32::from_rgb(0xfe, 0xbc, 0x2e));
        assert_eq!(Dot::Green.color(), Color32::from_rgb(0x28, 0xc8, 0x40));
        // The dots are the only colours the chrome adds: the strip's fill and its
        // hairline are the palette's.
        assert_eq!(Dot::Red.color(), Color32::from_rgb(255, 95, 87));
        assert_ne!(Dot::Red.color(), theme::ERROR);
        assert_ne!(Dot::Green.color(), theme::SUCCESS);
        assert_ne!(Dot::Yellow.color(), theme::NOTICE);
    }

    #[test]
    fn the_dot_labels_name_the_window_buttons() {
        assert_eq!(Dot::Red.label(), "Close window");
        assert_eq!(Dot::Yellow.label(), "Minimise window");
        assert_eq!(Dot::Green.label(), "Maximise window");
        // Three distinct names, so a screen reader and a test can tell them apart.
        let labels: std::collections::BTreeSet<&str> =
            Dot::ALL.iter().map(|dot| dot.label()).collect();
        assert_eq!(labels.len(), 3);
    }

    // ---------- the action seam ----------

    #[test]
    fn red_closes_yellow_minimises_and_green_maximises_then_restores() {
        // Red: the same close the platform's own button asked for.
        assert_eq!(command(Dot::Red.action(), false), ViewportCommand::Close);
        assert_eq!(
            command(Dot::Red.action(), false),
            command(Action::Close, true)
        );
        // Yellow: minimise.
        assert_eq!(
            command(Dot::Yellow.action(), false),
            ViewportCommand::Minimized(true)
        );
        // Green: maximise a normal window ...
        assert_eq!(
            command(Dot::Green.action(), false),
            ViewportCommand::Maximized(true)
        );
        // ... and restore a maximised one.
        assert_eq!(
            command(Dot::Green.action(), true),
            ViewportCommand::Maximized(false)
        );
        // Which is the same thing a double click on the strip does.
        assert_eq!(
            command(Action::ToggleMaximize, false),
            command(Dot::Green.action(), false)
        );
        assert_eq!(
            command(Action::ToggleMaximize, true),
            command(Dot::Green.action(), true)
        );
        assert_eq!(command(Action::Drag, false), ViewportCommand::StartDrag);
    }

    #[test]
    fn a_press_on_the_strip_drags_and_a_press_on_a_dot_does_not() {
        let window = window();
        // The strip, away from the dots: the platform drags the window.
        assert_eq!(
            press_action(window, pos2(400.0, 16.0), false),
            Some(Action::Drag)
        );
        // Under each dot, including its edge: the dot's action, never a drag.
        for dot in Dot::ALL {
            let centre = dot.centre();
            assert_eq!(
                press_action(window, centre, false),
                Some(dot.action()),
                "{:?} at {centre:?}",
                dot
            );
            assert_ne!(press_action(window, centre, false), Some(Action::Drag));
            assert_eq!(
                press_action(window, centre, true),
                Some(dot.action()),
                "a double click on a dot is still the dot's"
            );
        }
        // A double click on the strip maximises, or restores.
        assert_eq!(
            press_action(window, pos2(400.0, 16.0), true),
            Some(Action::ToggleMaximize)
        );
        // Below the strip nothing is the chrome's: the click reaches the app.
        assert_eq!(press_action(window, pos2(400.0, 40.0), false), None);
        assert_eq!(press_action(window, pos2(400.0, 40.0), true), None);
        // And neither is a point outside the window.
        assert_eq!(press_action(window, pos2(-4.0, 16.0), false), None);
        assert_eq!(
            press_action(window, pos2(4.0, WINDOW_HEIGHT + 4.0), false),
            None
        );
    }

    #[test]
    fn the_strip_band_stops_at_the_bottom_edge() {
        let window = window();
        // 31 px is still the strip, 33 px is the sidebar and the screen.
        assert_eq!(
            press_action(window, pos2(600.0, STRIP_HEIGHT - 0.5), false),
            Some(Action::Drag)
        );
        // A point below the band is the sidebar's and the screen's.
        assert_eq!(
            press_action(window, pos2(600.0, STRIP_HEIGHT + 0.5), false),
            None
        );
    }

    // ---------- the resize seam ----------

    #[test]
    fn the_eight_bands_are_6_px_wide_and_do_not_overlap() {
        let window = window();
        let bands = resize_rects(window);
        assert_eq!(bands.len(), 8);
        for (direction, rect) in bands {
            let horizontal = matches!(direction, ResizeDirection::North | ResizeDirection::South);
            let vertical = matches!(direction, ResizeDirection::East | ResizeDirection::West);
            if horizontal {
                // A top or bottom band: the full width, 6 px thick.
                assert_eq!(rect.height(), RESIZE_BAND, "{direction:?}");
                assert_eq!(rect.width(), WINDOW_WIDTH - 2.0 * RESIZE_BAND);
            } else if vertical {
                // A left or right band: the full height, 6 px thick.
                assert_eq!(rect.width(), RESIZE_BAND, "{direction:?}");
                assert_eq!(rect.height(), WINDOW_HEIGHT - 2.0 * RESIZE_BAND);
            } else {
                // A corner: 6 px by 6 px.
                assert_eq!(
                    (rect.width(), rect.height()),
                    (RESIZE_BAND, RESIZE_BAND),
                    "{direction:?} is not a 6 px square"
                );
            }
            // Each band is inside the window it belongs to, and touches the
            // edge it names.
            assert!(window.contains_rect(rect), "{direction:?}");
            let touches_top = matches!(
                direction,
                ResizeDirection::North | ResizeDirection::NorthWest | ResizeDirection::NorthEast
            );
            let touches_bottom = matches!(
                direction,
                ResizeDirection::South | ResizeDirection::SouthWest | ResizeDirection::SouthEast
            );
            let touches_left = matches!(
                direction,
                ResizeDirection::West | ResizeDirection::NorthWest | ResizeDirection::SouthWest
            );
            let touches_right = matches!(
                direction,
                ResizeDirection::East | ResizeDirection::NorthEast | ResizeDirection::SouthEast
            );
            assert_eq!(touches_top, rect.top() == 0.0, "{direction:?}");
            assert_eq!(
                touches_bottom,
                rect.bottom() == WINDOW_HEIGHT,
                "{direction:?}"
            );
            assert_eq!(touches_left, rect.left() == 0.0, "{direction:?}");
            assert_eq!(touches_right, rect.right() == WINDOW_WIDTH, "{direction:?}");
        }
        // A corner answers with the corner, an edge's middle with the edge.
        assert_eq!(
            resize_at(window, pos2(1.0, 1.0)),
            Some(ResizeDirection::NorthWest)
        );
        assert_eq!(
            resize_at(window, pos2(WINDOW_WIDTH - 1.0, 1.0)),
            Some(ResizeDirection::NorthEast)
        );
        assert_eq!(
            resize_at(window, pos2(1.0, WINDOW_HEIGHT - 1.0)),
            Some(ResizeDirection::SouthWest)
        );
        assert_eq!(
            resize_at(window, pos2(WINDOW_WIDTH - 1.0, WINDOW_HEIGHT - 1.0)),
            Some(ResizeDirection::SouthEast)
        );
        assert_eq!(
            resize_at(window, pos2(WINDOW_WIDTH * 0.5, 2.0)),
            Some(ResizeDirection::North)
        );
        assert_eq!(
            resize_at(window, pos2(WINDOW_WIDTH * 0.5, WINDOW_HEIGHT - 2.0)),
            Some(ResizeDirection::South)
        );
        assert_eq!(
            resize_at(window, pos2(2.0, 400.0)),
            Some(ResizeDirection::West)
        );
        assert_eq!(
            resize_at(window, pos2(WINDOW_WIDTH - 2.0, 400.0)),
            Some(ResizeDirection::East)
        );
        // The middle of the window is nobody's: the app keeps its clicks.
        assert_eq!(
            resize_at(window, pos2(WINDOW_WIDTH * 0.5, WINDOW_HEIGHT * 0.5)),
            None
        );
        assert_eq!(resize_at(window, pos2(20.0, 30.0)), None);
        // The bands only meet on their edges: no point *inside* two of them.
        // (A point exactly on the line where a corner ends and an edge band
        // begins is on both closed rects, which is how egui's rects work; it is
        // still one direction, because the corner is tested first.)
        for x in [0.5, 3.0, 600.0, WINDOW_WIDTH - 3.0, WINDOW_WIDTH - 0.5] {
            for y in [
                0.5,
                3.0,
                16.0,
                400.0,
                WINDOW_HEIGHT - 3.0,
                WINDOW_HEIGHT - 0.5,
            ] {
                let hits = resize_rects(window)
                    .into_iter()
                    .filter(|(_, rect)| rect.contains(pos2(x, y)))
                    .count();
                assert!(hits <= 1, "{x},{y} is on {hits} bands");
            }
        }
    }

    #[test]
    fn a_narrow_window_still_gets_eight_bands_and_no_panic() {
        // Smaller than two 6 px bands: the bands shrink to half the window, so
        // they meet in the middle instead of overlapping.
        let tiny = Rect::from_min_size(pos2(0.0, 0.0), vec2(8.0, 8.0));
        let bands = resize_rects(tiny);
        assert_eq!(bands.len(), 8);
        for (direction, rect) in bands {
            assert!(
                rect.width() <= 4.0 && rect.height() <= 4.0,
                "{direction:?}: {rect:?}"
            );
        }
        // A zero-size window has no band with an area: `resize_edges` skips
        // those rather than interact with a degenerate rect.
        let empty = Rect::from_min_size(pos2(0.0, 0.0), Vec2::ZERO);
        let bands = resize_rects(empty);
        assert_eq!(bands.len(), 8);
        assert!(
            bands
                .iter()
                .all(|(_, rect)| rect.width() <= 0.0 && rect.height() <= 0.0)
        );
    }

    #[test]
    fn a_resize_drag_moves_the_edge_under_the_pointer() {
        let inner = vec2(WINDOW_WIDTH, WINDOW_HEIGHT);
        let outer = pos2(100.0, 50.0);

        // East: the size grows, the window does not move.
        let (size, pos) = resize_drag(ResizeDirection::East, inner, outer, vec2(120.0, 0.0));
        assert_eq!(size, vec2(WINDOW_WIDTH + 120.0, WINDOW_HEIGHT));
        assert_eq!(pos, outer);

        // South: the same, downwards.
        let (size, pos) = resize_drag(ResizeDirection::South, inner, outer, vec2(0.0, 40.0));
        assert_eq!(size, vec2(WINDOW_WIDTH, WINDOW_HEIGHT + 40.0));
        assert_eq!(pos, outer);

        // West: the size grows leftwards and the window moves with the pointer,
        // so the right edge stays where it was.
        let (size, pos) = resize_drag(ResizeDirection::West, inner, outer, vec2(-60.0, 0.0));
        assert_eq!(size, vec2(WINDOW_WIDTH + 60.0, WINDOW_HEIGHT));
        assert_eq!(pos.x, outer.x - 60.0);
        assert_eq!(pos.x + size.x, outer.x + WINDOW_WIDTH);

        // North: the same, upwards.
        let (size, pos) = resize_drag(ResizeDirection::North, inner, outer, vec2(0.0, -30.0));
        assert_eq!(size, vec2(WINDOW_WIDTH, WINDOW_HEIGHT + 30.0));
        assert_eq!(pos.y, outer.y - 30.0);
        assert_eq!(pos.y + size.y, outer.y + WINDOW_HEIGHT);

        // A corner moves both.
        let (size, pos) = resize_drag(ResizeDirection::SouthEast, inner, outer, vec2(10.0, 20.0));
        assert_eq!(size, vec2(WINDOW_WIDTH + 10.0, WINDOW_HEIGHT + 20.0));
        assert_eq!(pos, outer);
    }

    #[test]
    fn a_resize_drag_stops_at_the_minimum_size() {
        let inner = vec2(WINDOW_WIDTH, WINDOW_HEIGHT);
        let outer = pos2(100.0, 50.0);
        // Dragged far past the floor: the window stops at MIN_WIDTH.
        let (size, pos) = resize_drag(ResizeDirection::East, inner, outer, vec2(-900.0, 0.0));
        assert_eq!(size.x, MIN_WIDTH);
        assert_eq!(pos, outer, "an east drag never moves the window");
        // And on the west edge the position follows the clamped size, so the
        // right edge is still where it was.
        let (size, pos) = resize_drag(ResizeDirection::West, inner, outer, vec2(900.0, 0.0));
        assert_eq!(size.x, MIN_WIDTH);
        assert_eq!(pos.x + size.x, outer.x + WINDOW_WIDTH);
        assert_eq!(pos.x, outer.x + (WINDOW_WIDTH - MIN_WIDTH));
        // The same floor vertically.
        let (size, _) = resize_drag(ResizeDirection::South, inner, outer, vec2(0.0, -2000.0));
        assert_eq!(size.y, MIN_HEIGHT);
        let (size, _) = resize_drag(ResizeDirection::North, inner, outer, vec2(0.0, 2000.0));
        assert_eq!(size.y, MIN_HEIGHT);
        // Under the minimum already: a drag never makes it smaller.
        let small = vec2(MIN_WIDTH, MIN_HEIGHT);
        let (size, _) = resize_drag(ResizeDirection::SouthEast, small, outer, vec2(-10.0, -10.0));
        assert_eq!(size, small);
    }

    #[test]
    fn every_edge_names_a_cursor() {
        for (direction, _) in resize_rects(window()) {
            let cursor = resize_cursor(direction);
            assert_ne!(cursor, CursorIcon::Default, "{direction:?}");
        }
        assert_eq!(
            resize_cursor(ResizeDirection::North),
            CursorIcon::ResizeVertical
        );
        assert_eq!(
            resize_cursor(ResizeDirection::South),
            CursorIcon::ResizeVertical
        );
        assert_eq!(
            resize_cursor(ResizeDirection::East),
            CursorIcon::ResizeHorizontal
        );
        assert_eq!(
            resize_cursor(ResizeDirection::West),
            CursorIcon::ResizeHorizontal
        );
        assert_eq!(
            resize_cursor(ResizeDirection::NorthWest),
            CursorIcon::ResizeNwSe
        );
        assert_eq!(
            resize_cursor(ResizeDirection::SouthEast),
            CursorIcon::ResizeNwSe
        );
        assert_eq!(
            resize_cursor(ResizeDirection::NorthEast),
            CursorIcon::ResizeNeSw
        );
        assert_eq!(
            resize_cursor(ResizeDirection::SouthWest),
            CursorIcon::ResizeNeSw
        );
    }

    #[test]
    fn the_platform_switch_is_the_one_the_module_note_explains() {
        // macOS has no native drag-resize in winit 0.30.13; everything else does.
        assert_eq!(NATIVE_DRAG_RESIZE, !cfg!(target_os = "macos"));
    }

    // ---------- the keyboard ----------

    /// What one frame with one key press asks the window for.
    fn ask(key: Key, modifiers: Modifiers) -> Option<Action> {
        let ctx = egui::Context::default();
        let input = egui::RawInput {
            modifiers,
            events: vec![egui::Event::Key {
                key,
                physical_key: None,
                pressed: true,
                repeat: false,
                modifiers,
            }],
            screen_rect: Some(Rect::from_min_size(
                Pos2::ZERO,
                vec2(WINDOW_WIDTH, WINDOW_HEIGHT),
            )),
            ..Default::default()
        };
        let mut asked = None;
        let _ = ctx.run(input, |ctx| asked = shortcut(ctx));
        asked
    }

    #[test]
    fn cmd_or_ctrl_w_and_q_close_the_window() {
        // `Modifiers::COMMAND` is Cmd on macOS and Ctrl elsewhere.
        assert_eq!(ask(Key::W, Modifiers::COMMAND), Some(Action::Close));
        assert_eq!(ask(Key::Q, Modifiers::COMMAND), Some(Action::Close));
        assert_eq!(
            command(Action::Close, false),
            ViewportCommand::Close,
            "and closing is the close the platform's button asked for"
        );
        // A bare W or Q is typing, not the window's: the app keeps its keys.
        assert_eq!(ask(Key::W, Modifiers::NONE), None);
        assert_eq!(ask(Key::Q, Modifiers::NONE), None);
        // Another letter with the command key is not the window's either.
        assert_eq!(ask(Key::E, Modifiers::COMMAND), None);
    }

    #[test]
    fn the_keyboard_says_nothing_when_no_key_is_pressed() {
        let ctx = egui::Context::default();
        let mut asked = Some(Action::Close);
        let _ = ctx.run(egui::RawInput::default(), |ctx| asked = shortcut(ctx));
        assert_eq!(asked, None);
    }

    // ---------- the strip as a widget ----------

    /// The strip drawn in a real `egui` pass: the three dots are widgets with
    /// the buttons' names, in the top 32 px and 12 px square, and a click on one
    /// of them asks for that dot's action.
    ///
    /// This is the other half of the seam: the pure functions above decide, and
    /// this shows the drawing reaching them — the dot rects the hit test uses
    /// are the rects the widgets get.
    #[test]
    fn a_click_on_a_dot_asks_for_that_dot_s_action() {
        use egui_kittest::Harness;
        use egui_kittest::kittest::Queryable;

        let mut harness = Harness::builder()
            .with_size(vec2(WINDOW_WIDTH, WINDOW_HEIGHT))
            .with_pixels_per_point(1.0)
            .build_state(
                |ctx, asked: &mut Vec<Action>| {
                    crate::app::apply_style_once(ctx);
                    if let Some(action) = strip(ctx) {
                        asked.push(action);
                    }
                },
                Vec::new(),
            );
        harness.run_steps(2);

        // Each dot is a widget at the pixels the geometry names.
        for dot in Dot::ALL {
            let node = harness.get_by_label(dot.label());
            let rect = node.rect();
            assert_eq!(rect.min, dot.rect().min, "{:?}", dot);
            assert_eq!(rect.size(), vec2(DOT_SIZE, DOT_SIZE), "{:?}", dot);
            assert!(rect.bottom() <= STRIP_HEIGHT, "{:?}", dot);
        }

        // A click reaches the strip as that dot's action.
        harness.get_by_label(Dot::Red.label()).click();
        harness.run();
        harness.get_by_label(Dot::Yellow.label()).click();
        harness.run();
        harness.get_by_label(Dot::Green.label()).click();
        harness.run();
        for dot in Dot::ALL {
            assert!(
                harness.state().contains(&dot.action()),
                "{:?} never asked for {:?}: {:?}",
                dot,
                dot.action(),
                harness.state()
            );
            assert!(
                !NATIVE_DRAG_RESIZE || !harness.state().contains(&Action::Drag),
                "a click on a dot must not drag the window"
            );
        }
    }
}
