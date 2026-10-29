//! The Watch Folders screen.
//!
//! The webview's table (`#view-watch .watch-grid` in `index.html`): a header with
//! "Add", then one 56 px row per watched folder — the folder and its path, the
//! Frame.io destination with the direction icon, the status dot and word, the
//! file count, the last transfer, and the row's actions (pause/resume, edit,
//! show in Finder, remove). The empty state is "No watch folders".
//!
//! The shell (`crate::app`) supplies the `.view` padding, the scroll area and
//! the page head; the tokens come from [`crate::theme`].

use crate::model::{UiState, WatchRow};
use crate::theme;
use egui::{
    Align, Color32, CornerRadius, Layout, Painter, Rect, RichText, Sense, Shape, Stroke, StrokeKind,
    WidgetInfo, WidgetType, pos2, vec2,
};

/// The sidebar's width: the stylesheet's media queries are about the *window*,
/// and a screen only knows the main column it was given.
const SIDEBAR_W: f32 = theme::size::SIDEBAR_W;

// `.list-head { height: 32px }`, `.row-main { min-height: 56px }`.
const HEAD_HEIGHT: f32 = theme::size::LIST_HEAD_H;
/// `.row-main { min-height: 56px }` plus its own `border-bottom: 1px`, so a
/// row measures 57 px from border to border.
const ROW_HEIGHT: f32 = theme::size::ROW_H + 1.0;
/// `.row, .list-head { padding: 0 12px }`, `.watch-grid { gap: 12px }`.
const ROW_PADDING_X: f32 = theme::space::ROW_X;
const COLUMN_GAP: f32 = theme::space::XL;
/// `.watch-grid`: `minmax(0, 1.3fr) minmax(0, 1fr) 150px 140px 124px 150px`.
const STATUS_WIDTH: f32 = 150.0;
const FILES_WIDTH: f32 = 140.0;
const LAST_WIDTH: f32 = 124.0;
const ACTIONS_WIDTH: f32 = 150.0;
const MIN_NAME_WIDTH: f32 = 140.0;
const MIN_DEST_WIDTH: f32 = 110.0;

/// `WATCH_STATUS` in app.js, for the two states a [`WatchRow`] can be in.
const STATUS_WATCHING: &str = "Watching";
const STATUS_PAUSED: &str = "Paused";

/// What the screen asks the shell to do.
///
/// A view is a pure renderer over [`UiState`], so everything that needs the
/// disk or Frame.io is reported back instead of called: the shell turns these
/// into `call('PickWatchFolder')`, `call('SetWatchEnabled', …)` and so on.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Action {
    /// "Add": pick a folder, then choose its Frame.io destination.
    AddWatch,
    /// The pause/resume button; `index` points into `state.watches`.
    SetEnabled { index: usize, enabled: bool },
    /// Open the watch folder editor (`.w-edit`).
    Edit(usize),
    /// Remove the watch folder (`.w-remove`).
    Remove(usize),
    /// Show the folder in Finder (`.w-reveal`).
    Reveal(usize),
}

/// The screen, for a shell that does not wire the actions yet.
pub fn show(ui: &mut egui::Ui, state: &mut UiState) {
    let _ = show_with_actions(ui, state);
}

/// The screen, reporting at most one [`Action`] per frame.
pub fn show_with_actions(ui: &mut egui::Ui, state: &mut UiState) -> Option<Action> {
    let mut action = None;
    // The stylesheet's blocks abut: `.list { margin-top: 20px }`, and a row's
    // only gap is its 1 px border. The shell's 8 px item spacing would add a
    // gap to every one of those, so it is zeroed here and the gaps are explicit.
    ui.spacing_mut().item_spacing = vec2(theme::space::M, 0.0);

    crate::app::page_head(ui, "Watch Folders", |ui| {
        if primary_button(ui, "Add") {
            action = Some(Action::AddWatch);
        }
    });

    // `.list { margin-top: 20px }`
    ui.add_space(theme::space::HUGE);

    if state.watches.is_empty() {
        empty_state(ui, "No watch folders");
        return action;
    }

    let columns = Columns::for_width(ui.available_width());
    list_head(ui, &columns);
    for (index, watch) in state.watches.iter_mut().enumerate() {
        watch_row(ui, index, watch, &state.home, &columns, &mut action);
    }

    action
}

/// `.watch-grid`, with the columns the stylesheet drops on a narrow window.
#[derive(Debug, Clone, Copy, PartialEq)]
struct Columns {
    name: f32,
    dest: f32,
    status: f32,
    files: Option<f32>,
    last: Option<f32>,
    actions: f32,
}

impl Columns {
    fn for_width(available: f32) -> Self {
        // `@media (max-width: 1279px) { .watch-grid > .w-last { display: none } }`
        // and `@media (max-width: 1099px) { … > .w-uploaded { display: none } }`.
        let window = available + SIDEBAR_W;
        let files = if window > 1099.0 { Some(FILES_WIDTH) } else { None };
        let last = if window > 1279.0 { Some(LAST_WIDTH) } else { None };

        let mut fixed = STATUS_WIDTH + ACTIONS_WIDTH;
        if let Some(width) = files {
            fixed += width;
        }
        if let Some(width) = last {
            fixed += width;
        }
        let mut gaps = 5.0 * COLUMN_GAP;
        if files.is_none() {
            gaps -= COLUMN_GAP;
        }
        if last.is_none() {
            gaps -= COLUMN_GAP;
        }

        // 1.3fr for the folder, 1fr for the destination, in what is left.
        let free = (available - ROW_PADDING_X * 2.0 - fixed - gaps).max(MIN_NAME_WIDTH + MIN_DEST_WIDTH);
        let name = free * 1.3 / 2.3;
        let dest = free - name;

        Columns {
            name: name.max(MIN_NAME_WIDTH),
            dest: dest.max(MIN_DEST_WIDTH),
            status: STATUS_WIDTH,
            files,
            last,
            actions: ACTIONS_WIDTH,
        }
    }
}

/// `.list-head`: the dim 11.5 px row of column names.
fn list_head(ui: &mut egui::Ui, columns: &Columns) {
    strip(ui, HEAD_HEIGHT, false, |ui| {
        head_cell(ui, columns.name, false, "Folder");
        head_cell(ui, columns.dest, false, "Frame.io");
        head_cell(ui, columns.status, false, "Status");
        if let Some(width) = columns.files {
            head_cell(ui, width, true, "Files");
        }
        if let Some(width) = columns.last {
            head_cell(ui, width, false, "Last transfer");
        }
        cell(ui, columns.actions, HEAD_HEIGHT, false, |_ui| {});
    });
}

fn head_cell(ui: &mut egui::Ui, width: f32, right: bool, text: &str) {
    cell(ui, width, HEAD_HEIGHT, right, |ui| {
        ui.add(theme::truncating(
            RichText::new(text)
                .font(theme::caption())
                .color(theme::TEXT_3)
                .strong(),
        ));
    });
}

/// One watched folder (`.row .row-main`).
fn watch_row(
    ui: &mut egui::Ui,
    index: usize,
    watch: &mut WatchRow,
    home: &str,
    columns: &Columns,
    action: &mut Option<Action>,
) {
    let name = base_name(&watch.local);
    let path = short_path(&watch.local, home);
    let verb = direction_verb(&watch.direction);
    let status = if watch.enabled { STATUS_WATCHING } else { STATUS_PAUSED };

    strip(ui, ROW_HEIGHT, true, |ui| {
        // Folder: the 30 px tile, then the name and the shortened path.
        cell(ui, columns.name, ROW_HEIGHT, false, |ui| {
            ui.spacing_mut().item_spacing.x = theme::space::XL;
            icon_tile(ui, Glyph::Folder);
            ui.vertical(|ui| {
                ui.spacing_mut().item_spacing.y = 1.0;
                ui.add(theme::truncating(
                    RichText::new(&name)
                        .font(theme::body())
                        .color(if watch.enabled { theme::TEXT } else { theme::TEXT_2 })
                        .strong(),
                ))
                .on_hover_text(&watch.local);
                ui.add(theme::truncating(
                    RichText::new(&path)
                        .font(theme::detail())
                        .color(theme::TEXT_3),
                ))
                .on_hover_text(&watch.local);
            });
        });

        // Frame.io: the direction icon and the destination label.
        cell(ui, columns.dest, ROW_HEIGHT, false, |ui| {
            ui.spacing_mut().item_spacing.x = 7.0;
            glyph(ui, Glyph::of_direction(&watch.direction), theme::TEXT_3);
            ui.add(theme::truncating(
                RichText::new(&watch.label)
                    .font(theme::body())
                    .color(theme::TEXT_2),
            ))
            .on_hover_text(format!("{verb} {}", watch.label));
        });

        // Status: the dot and the word (`.status.watching` / `.status.paused`).
        cell(ui, columns.status, ROW_HEIGHT, false, |ui| {
            let key = if watch.enabled { "watching" } else { "paused" };
            dot(ui, theme::status_color(key));
            ui.add(theme::truncating(
                RichText::new(status)
                    .font(theme::note())
                    .color(theme::TEXT_2),
            ));
        });

        if let Some(width) = columns.files {
            cell(ui, width, ROW_HEIGHT, true, |ui| {
                ui.add(theme::truncating(
                    RichText::new(count_word(watch.pending))
                        .font(theme::body())
                        .color(theme::TEXT_2),
                ))
                .on_hover_text(format!("{} pending", watch.pending));
            });
        }

        if let Some(width) = columns.last {
            cell(ui, width, ROW_HEIGHT, false, |ui| {
                // `WatchRow` carries no last-transfer time; the webview prints
                // an em dash when there is none.
                ui.label(
                    RichText::new("—")
                        .font(theme::body())
                        .color(theme::TEXT_2),
                );
            });
        }

        // Actions: pause/resume, edit, show in Finder, remove.
        cell(ui, columns.actions, ROW_HEIGHT, false, |ui| {
            ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                ui.spacing_mut().item_spacing.x = 2.0;
                if icon_button(ui, Glyph::Trash, "Remove") {
                    *action = Some(Action::Remove(index));
                }
                if icon_button(ui, Glyph::Reveal, "Show in Finder") {
                    *action = Some(Action::Reveal(index));
                }
                if icon_button(ui, Glyph::Pencil, "Edit") {
                    *action = Some(Action::Edit(index));
                }
                let (glyph, label) = if watch.enabled {
                    (Glyph::Pause, "Pause")
                } else {
                    (Glyph::Play, "Resume")
                };
                if icon_button(ui, glyph, label) {
                    let enabled = !watch.enabled;
                    watch.enabled = enabled;
                    *action = Some(Action::SetEnabled { index, enabled });
                }
            });
        });
    });
}

// ---------- the pieces of the table ----------

/// One full-width strip: the hover wash, the row padding, the 1 px bottom
/// border, and a child `Ui` laid out left to right.
fn strip(ui: &mut egui::Ui, height: f32, hoverable: bool, add: impl FnOnce(&mut egui::Ui)) {
    let width = ui.available_width().max(0.0);
    let (rect, response) = ui.allocate_exact_size(vec2(width, height), Sense::hover());
    if hoverable && response.hovered() {
        ui.painter()
            .rect_filled(rect, CornerRadius::ZERO, theme::ROW_HOVER);
    }

    let mut child = ui.new_child(
        egui::UiBuilder::new()
            .max_rect(rect.shrink2(vec2(ROW_PADDING_X, 0.0)))
            .layout(Layout::left_to_right(Align::Center)),
    );
    child.spacing_mut().item_spacing.x = 0.0;
    add(&mut child);

    // The border is the last pixel *inside* the strip (`border-bottom`), so the
    // next row starts on the following pixel and the pitch is 57.
    ui.painter().hline(
        rect.x_range(),
        rect.bottom() - 1.0,
        theme::hairline(theme::BORDER),
    );
}

/// A fixed-width column of a row.
fn cell(ui: &mut egui::Ui, width: f32, height: f32, right: bool, add: impl FnOnce(&mut egui::Ui)) {
    let (rect, _) = ui.allocate_exact_size(vec2(width, height), Sense::hover());
    let layout = if right {
        Layout::right_to_left(Align::Center)
    } else {
        Layout::left_to_right(Align::Center)
    };
    let mut child = ui.new_child(egui::UiBuilder::new().max_rect(rect).layout(layout));
    child.spacing_mut().item_spacing.x = COLUMN_GAP;
    add(&mut child);
    ui.add_space(COLUMN_GAP);
}

/// `.status .dot`: 7 px, in the status colour.
fn dot(ui: &mut egui::Ui, colour: Color32) {
    let (rect, _) = ui.allocate_exact_size(vec2(theme::size::DOT, theme::size::DOT), Sense::hover());
    ui.painter()
        .circle_filled(rect.center(), theme::size::DOT * 0.5, colour);
    ui.add_space(7.0);
}

/// `.kind-icon`: a 30 px `--card` tile with the folder outline in it.
fn icon_tile(ui: &mut egui::Ui, glyph: Glyph) {
    let size = theme::size::KIND_ICON;
    let (rect, _) = ui.allocate_exact_size(vec2(size, size), Sense::hover());
    let painter = ui.painter();
    painter.rect_filled(rect, theme::R_S, theme::CARD);
    painter.rect_stroke(rect, theme::R_S, theme::stroke_border(), StrokeKind::Inside);
    paint(painter, rect, glyph, theme::TEXT_2, 16.0);
}

/// A bare 16 px glyph, for the destination's direction icon.
fn glyph(ui: &mut egui::Ui, glyph: Glyph, colour: Color32) {
    let (rect, _) = ui.allocate_exact_size(vec2(14.0, 14.0), Sense::hover());
    paint(ui.painter(), rect, glyph, colour, 14.0);
}

/// `.icon-btn`: 28 x 28, transparent, `--text-3` until hovered. The button is
/// painted, and `WidgetInfo` gives it the name a screen reader and a test use.
fn icon_button(ui: &mut egui::Ui, glyph: Glyph, label: &str) -> bool {
    let size = vec2(theme::size::ICON_BTN, theme::size::ICON_BTN);
    let (rect, response) = ui.allocate_exact_size(size, Sense::click());
    let response = response.on_hover_text(label);
    let colour = if response.hovered() {
        theme::TEXT
    } else {
        theme::TEXT_3
    };
    if response.hovered() {
        ui.painter().rect_filled(
            rect,
            CornerRadius::same(theme::R_S_PX),
            if response.is_pointer_button_down_on() {
                theme::ACTIVE
            } else {
                theme::HOVER
            },
        );
    }
    paint(ui.painter(), rect, glyph, colour, 16.0);
    response.widget_info(|| WidgetInfo::labeled(WidgetType::Button, ui.is_enabled(), label));
    response.clicked()
}

/// The glyphs the row draws. `I.<name>` in app.js are the same shapes as SVG.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Glyph {
    Folder,
    Up,
    Down,
    Sync,
    Pause,
    Play,
    Pencil,
    Reveal,
    Trash,
}

impl Glyph {
    /// `DIR_ICON` in app.js: `upload` is an up arrow, `download` a down arrow,
    /// `mirror` the two-arrow sync mark.
    fn of_direction(direction: &str) -> Glyph {
        match direction {
            "download" => Glyph::Down,
            "mirror" => Glyph::Sync,
            _ => Glyph::Up,
        }
    }
}

/// Paint `glyph` inside `rect`, on a canvas `size` px tall.
fn paint(painter: &Painter, rect: Rect, glyph: Glyph, colour: Color32, size: f32) {
    let c = rect.center();
    let s = size * 0.5;
    let stroke = Stroke::new(1.4_f32, colour);
    match glyph {
        Glyph::Folder => {
            // A tab on the left shoulder, then the body.
            let body = Rect::from_center_size(c, vec2(s * 1.8, s * 1.4));
            let tab = Rect::from_min_size(
                pos2(body.left(), body.top() - s * 0.35),
                vec2(s * 0.8, s * 0.35),
            );
            painter.rect_stroke(body, CornerRadius::same(2), stroke, StrokeKind::Inside);
            painter.line_segment([tab.left_bottom(), tab.left_top()], stroke);
            painter.line_segment([tab.left_top(), tab.right_top()], stroke);
            painter.line_segment([tab.right_top(), tab.right_bottom()], stroke);
        }
        Glyph::Up | Glyph::Down => {
            let dir = if glyph == Glyph::Up { -1.0 } else { 1.0 };
            let tip = pos2(c.x, c.y + dir * s * 0.9);
            let tail = pos2(c.x, c.y - dir * s * 0.9);
            painter.line_segment([tail, tip], stroke);
            let wing = s * 0.45;
            painter.line_segment(
                [pos2(tip.x - wing, tip.y - dir * wing), tip],
                stroke,
            );
            painter.line_segment(
                [pos2(tip.x + wing, tip.y - dir * wing), tip],
                stroke,
            );
        }
        Glyph::Sync => {
            // Two arrows, one each way.
            let y = s * 0.45;
            painter.line_segment([pos2(c.x - s * 0.9, c.y - y), pos2(c.x + s * 0.9, c.y - y)], stroke);
            painter.line_segment([pos2(c.x + s * 0.9, c.y + y), pos2(c.x - s * 0.9, c.y + y)], stroke);
            let w = s * 0.4;
            painter.line_segment(
                [pos2(c.x + s * 0.9 - w, c.y - y - w), pos2(c.x + s * 0.9, c.y - y)],
                stroke,
            );
            painter.line_segment(
                [pos2(c.x - s * 0.9 + w, c.y + y + w), pos2(c.x - s * 0.9, c.y + y)],
                stroke,
            );
        }
        Glyph::Pause => {
            let y = s * 0.6;
            for x in [-s * 0.3, s * 0.3] {
                painter.line_segment([pos2(c.x + x, c.y - y), pos2(c.x + x, c.y + y)], stroke);
            }
        }
        Glyph::Play => {
            let shape = Shape::convex_polygon(
                vec![
                    pos2(c.x - s * 0.35, c.y - s * 0.6),
                    pos2(c.x - s * 0.35, c.y + s * 0.6),
                    pos2(c.x + s * 0.55, c.y),
                ],
                colour,
                Stroke::NONE,
            );
            painter.add(shape);
        }
        Glyph::Pencil => {
            // The stylesheet's `I.pencil`: a shaft along the diagonal with a
            // pointed tip at the lower left and the eraser line across the top.
            // (The previous two strokes read as a golf club at 18 px.)
            let map = |x: f32, y: f32| {
                pos2(c.x + (x - 10.0) / 10.0 * s, c.y + (y - 10.0) / 10.0 * s)
            };
            painter.add(egui::Shape::closed_line(
                vec![map(4.0, 16.0), map(4.7, 12.8), map(13.3, 4.2), map(15.3, 6.2), map(6.7, 14.8)],
                stroke,
            ));
            painter.line_segment([map(12.8, 4.7), map(15.3, 7.2)], stroke);
        }
        Glyph::Reveal => {
            painter.circle_stroke(
                pos2(c.x - s * 0.2, c.y - s * 0.2),
                s * 0.45,
                stroke,
            );
            painter.line_segment(
                [pos2(c.x + s * 0.12, c.y + s * 0.12), pos2(c.x + s * 0.6, c.y + s * 0.6)],
                stroke,
            );
        }
        Glyph::Trash => {
            let body = Rect::from_center_size(
                pos2(c.x, c.y + s * 0.12),
                vec2(s * 1.1, s * 1.3),
            );
            painter.rect_stroke(body, CornerRadius::same(2), stroke, StrokeKind::Inside);
            painter.line_segment(
                [pos2(c.x - s * 0.7, body.top()), pos2(c.x + s * 0.7, body.top())],
                stroke,
            );
            painter.line_segment(
                [
                    pos2(c.x - s * 0.25, body.top() - s * 0.25),
                    pos2(c.x + s * 0.25, body.top() - s * 0.25),
                ],
                stroke,
            );
        }
    }
}

/// `DIR_VERB` in app.js.
fn direction_verb(direction: &str) -> &'static str {
    match direction {
        "download" => "Downloads from",
        "mirror" => "Mirrors",
        _ => "Uploads to",
    }
}

/// The number in the Files column. The webview counts what a folder has moved;
/// a [`WatchRow`] carries what is still to move.
fn count_word(pending: i64) -> String {
    match pending {
        n if n < 0 => "—".to_string(),
        n => n.to_string(),
    }
}

/// `baseName` in app.js: the last path component.
pub fn base_name(path: &str) -> String {
    let trimmed = path.trim().trim_end_matches('/');
    match trimmed.rsplit('/').next() {
        Some(name) if !name.is_empty() => name.to_string(),
        _ => trimmed.to_string(),
    }
}

/// `shortPath` in app.js: `~` for the home folder, the path unchanged otherwise.
pub fn short_path(path: &str, home: &str) -> String {
    let path = path.trim();
    let home = home.trim().trim_end_matches('/');
    if path.is_empty() {
        return String::new();
    }
    if !home.is_empty() && (path == home || path.starts_with(&format!("{home}/"))) {
        format!("~{}", &path[home.len()..])
    } else {
        path.to_string()
    }
}

/// `.btn.primary`: `--brand` fill, white text, 32 px tall.
///
/// The fill goes on the widget states rather than on the button, so `:hover`
/// still brightens to `--brand-hover`.
fn primary_button(ui: &mut egui::Ui, text: &str) -> bool {
    let mut clicked = false;
    ui.scope(|ui| {
        {
            let widgets = &mut ui.style_mut().visuals.widgets;
            widgets.inactive.weak_bg_fill = theme::BRAND;
            widgets.hovered.weak_bg_fill = theme::BRAND_HOVER;
            widgets.active.weak_bg_fill = theme::BRAND_HOVER;
            widgets.inactive.bg_stroke = Stroke::NONE;
            widgets.hovered.bg_stroke = Stroke::NONE;
            widgets.active.bg_stroke = Stroke::NONE;
            widgets.inactive.fg_stroke = theme::hairline(theme::WHITE);
            widgets.hovered.fg_stroke = theme::hairline(theme::WHITE);
            widgets.active.fg_stroke = theme::hairline(theme::WHITE);
        }
        clicked = ui
            .add(
                egui::Button::new(
                    RichText::new(text)
                        .font(theme::body())
                        .color(theme::WHITE),
                )
                .min_size(vec2(0.0, theme::size::CONTROL_H)),
            )
            .clicked();
    });
    clicked
}

/// `.empty`: 72 px of air, a 44 px icon tile, then the title.
fn empty_state(ui: &mut egui::Ui, heading: &str) {
    ui.add_space(72.0);
    ui.vertical_centered(|ui| {
        ui.spacing_mut().item_spacing.y = theme::space::XL;
        let (rect, _) = ui.allocate_exact_size(vec2(44.0, 44.0), Sense::hover());
        let radius = CornerRadius::same(10);
        let painter = ui.painter();
        painter.rect_filled(rect, radius, theme::CARD);
        painter.rect_stroke(rect, radius, theme::stroke_border(), StrokeKind::Inside);
        // The webview draws an eye here; two circles read the same at 22 px.
        painter.circle_stroke(rect.center(), 8.0, Stroke::new(1.4_f32, theme::TEXT_3));
        painter.circle_filled(rect.center(), 2.4, theme::TEXT_3);

        ui.label(
            RichText::new(heading)
                .font(theme::section())
                .color(theme::TEXT)
                .strong(),
        );
    });
}

#[cfg(test)]
mod tests {
    use super::*;
    use egui_kittest::Harness;
    use egui_kittest::kittest::Queryable;

    fn watch(local: &str, label: &str, direction: &str, enabled: bool, pending: i64) -> WatchRow {
        WatchRow {
            local: local.to_string(),
            label: label.to_string(),
            direction: direction.to_string(),
            enabled,
            pending,
        }
    }

    fn state_with(watches: Vec<WatchRow>) -> UiState {
        UiState {
            watches,
            home: "/Users/chaos".to_string(),
            ..UiState::default()
        }
    }

    /// The main column of the default 1180 px window (224 px sidebar), which is
    /// the width the webview's own screenshot is taken at.
    fn screen_harness(state: UiState) -> Harness<'static, UiState> {
        Harness::builder()
            .with_size(vec2(956.0, 780.0))
            .build_state(
                |ctx, state: &mut UiState| {
                    crate::app::apply_style_once(ctx);
                    egui::CentralPanel::default().show(ctx, |ui| show(ui, state));
                },
                state,
            )
    }

    #[test]
    fn the_empty_state_says_no_watch_folders() {
        let harness = screen_harness(state_with(Vec::new()));
        harness.get_by_label("No watch folders");
        harness.get_by_label("Add");
        assert!(
            harness.query_by_label("Folder").is_none(),
            "the webview hides the table header when there is nothing to list"
        );
    }

    #[test]
    fn the_header_has_the_webview_columns() {
        let harness = screen_harness(state_with(vec![watch(
            "/Users/chaos/Movies/Rushes",
            "Rushes 2026",
            "upload",
            true,
            3,
        )]));
        for head in ["Folder", "Frame.io", "Status", "Files"] {
            harness.get_by_label(head);
        }
        // `@media (max-width: 1279px)` drops "Last transfer" at this width.
        assert!(harness.query_by_label("Last transfer").is_none());
    }

    #[test]
    fn a_row_shows_the_folder_its_path_the_destination_and_the_status() {
        let harness = screen_harness(state_with(vec![watch(
            "/Users/chaos/Movies/Rushes",
            "Rushes 2026",
            "upload",
            true,
            3,
        )]));

        harness.get_by_label("Rushes");
        harness.get_by_label("~/Movies/Rushes");
        harness.get_by_label("Rushes 2026");
        harness.get_by_label("Watching");
        harness.get_by_label("3");
        harness.get_by_label("Pause");
        harness.get_by_label("Edit");
        harness.get_by_label("Show in Finder");
        harness.get_by_label("Remove");
    }

    #[test]
    fn a_paused_mirror_row_reads_paused_and_mirrors() {
        let harness = screen_harness(state_with(vec![
            watch("/Users/chaos/Movies/Archive", "Archive", "mirror", false, 0),
            watch("/Users/chaos/Movies/In", "Inbox", "download", true, 12),
        ]));

        harness.get_by_label("Paused");
        harness.get_by_label("Resume");
        harness.get_by_label("Inbox");
        harness.get_by_label("12");
    }

    #[test]
    fn the_pause_button_flips_the_watch() {
        let mut harness = screen_harness(state_with(vec![watch(
            "/Users/chaos/Movies/Rushes",
            "Rushes 2026",
            "upload",
            true,
            0,
        )]));

        assert!(harness.state().watches[0].enabled);
        harness.get_by_label("Pause").click();
        harness.run();

        assert!(!harness.state().watches[0].enabled);
        harness.get_by_label("Resume");
    }

    #[test]
    fn add_is_reported_as_an_action() {
        let seen: std::rc::Rc<std::cell::RefCell<Vec<Action>>> = Default::default();
        let sink = seen.clone();
        let mut harness = Harness::builder()
            .with_size(vec2(956.0, 780.0))
            .build_state(
                move |ctx, state: &mut UiState| {
                    egui::CentralPanel::default().show(ctx, |ui| {
                        if let Some(action) = show_with_actions(ui, state) {
                            sink.borrow_mut().push(action);
                        }
                    });
                },
                state_with(Vec::new()),
            );

        harness.get_by_label("Add").click();
        harness.run();

        assert!(
            seen.borrow().contains(&Action::AddWatch),
            "actions seen: {:?}",
            seen.borrow()
        );
    }

    #[test]
    fn removing_the_second_row_reports_its_index() {
        let seen: std::rc::Rc<std::cell::RefCell<Vec<Action>>> = Default::default();
        let sink = seen.clone();
        let mut harness = Harness::builder()
            .with_size(vec2(956.0, 780.0))
            .build_state(
                move |ctx, state: &mut UiState| {
                    egui::CentralPanel::default().show(ctx, |ui| {
                        if let Some(action) = show_with_actions(ui, state) {
                            sink.borrow_mut().push(action);
                        }
                    });
                },
                state_with(vec![
                    watch("/Users/chaos/Movies/One", "One", "upload", true, 0),
                    watch("/Users/chaos/Movies/Two", "Two", "upload", true, 0),
                ]),
            );

        {
            let removals: Vec<_> = harness.get_all_by_label("Remove").collect();
            if let Some(second) = removals.last() {
                second.click();
            }
        }
        harness.run();

        assert!(
            seen.borrow().contains(&Action::Remove(1)),
            "actions seen: {:?}",
            seen.borrow()
        );
    }

    #[test]
    fn the_stylesheet_drops_columns_as_the_window_narrows() {
        // 1180 px window: Files stays, Last transfer is dropped (> 1279 needed).
        let default = Columns::for_width(956.0);
        assert!(default.files.is_some());
        assert!(default.last.is_none());

        // 1400 px window: both are shown.
        let wide = Columns::for_width(1176.0);
        assert!(wide.files.is_some());
        assert!(wide.last.is_some());

        // 900 px window: both are dropped.
        let narrow = Columns::for_width(676.0);
        assert!(narrow.files.is_none());
        assert!(narrow.last.is_none());

        // The folder and destination columns never collapse below their minimum.
        for width in [300.0, 500.0, 676.0, 956.0, 1600.0] {
            let columns = Columns::for_width(width);
            assert!(columns.name >= MIN_NAME_WIDTH);
            assert!(columns.dest >= MIN_DEST_WIDTH);
        }
    }

    #[test]
    fn paths_are_shortened_the_way_the_webview_does() {
        assert_eq!(base_name("/Users/chaos/Movies/Rushes"), "Rushes");
        assert_eq!(base_name("/Users/chaos/Movies/Rushes/"), "Rushes");
        assert_eq!(base_name("Rushes"), "Rushes");
        assert_eq!(base_name(""), "");
        assert_eq!(short_path("/Users/chaos/Movies", "/Users/chaos"), "~/Movies");
        assert_eq!(short_path("/Users/chaos", "/Users/chaos"), "~");
        assert_eq!(short_path("/Volumes/Raid", "/Users/chaos"), "/Volumes/Raid");
        // No home in the state: the path is shown whole.
        assert_eq!(short_path("/Volumes/Raid", ""), "/Volumes/Raid");
        assert_eq!(short_path("", "/Users/chaos"), "");
    }

    #[test]
    fn every_direction_has_a_verb_and_an_icon() {
        assert_eq!(direction_verb("upload"), "Uploads to");
        assert_eq!(direction_verb("download"), "Downloads from");
        assert_eq!(direction_verb("mirror"), "Mirrors");
        assert_eq!(direction_verb("something-else"), "Uploads to");
        assert_eq!(Glyph::of_direction("upload"), Glyph::Up);
        assert_eq!(Glyph::of_direction("download"), Glyph::Down);
        assert_eq!(Glyph::of_direction("mirror"), Glyph::Sync);
        assert_eq!(Glyph::of_direction(""), Glyph::Up);
    }

    #[test]
    fn a_very_long_path_is_elided_but_kept() {
        let long = format!("/Users/chaos/{}", "a-very-long-folder-name/".repeat(40));
        let harness = screen_harness(state_with(vec![watch(&long, &"Label ".repeat(60), "upload", true, 999)]));
        harness.get_by_label("999");
        // The sub line carries the whole path, shortened to `~`.
        let shortened = short_path(&long, "/Users/chaos");
        harness.get_by_label(&shortened);
    }
}
