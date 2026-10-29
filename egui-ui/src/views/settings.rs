//! The Settings screen.
//!
//! The four sections `frontend/dist/index.html` puts under `#view-settings`, in
//! its order and with its words: "Frame.io account" (sign-in state, "Adobe
//! Client ID", the setup steps with the redirect URI and "Copy"), "App" ("Open
//! at login", "Version" + "Check now", "Check for updates"), "Performance"
//! ("Parallel streams", "Chunk size") and "Storage" ("Download location" +
//! "Change…", "Save log with media", "App log" + "Show in Finder"), then the
//! `.about` line.
//!
//! The shell (`crate::app`) supplies the `.view` padding, the scroll area and
//! the page head; the tokens come from [`crate::theme`]. `.settings
//! { max-width: 680px }` is applied here because it is this screen's own measure.

use crate::model::UiState;
use crate::theme;
use egui::{
    Align, Checkbox, CollapsingHeader, Color32, ComboBox, CornerRadius, Frame, Layout, Margin,
    RichText, Slider, Stroke, TextEdit, vec2,
};

/// `.settings { max-width: 680px }`.
const SETTINGS_MAX_WIDTH: f32 = 680.0;
/// `.set-section { margin-bottom: 28px }` and `.set-card { margin-top: 12px }`.
const SECTION_GAP: f32 = 28.0;
const CARD_GAP: f32 = theme::space::XL;
/// `.set-line { padding: 14px 16px }`.
const LINE_PADDING: Margin = Margin::symmetric(theme::space::XXXL as i8, theme::space::XXL as i8);
/// The stylesheet sets `line-height: 1.45` on the body, so a 13 px label
/// occupies 18.85 px and a 12 px `.help` line 17.4 px. egui sizes a label to the
/// glyphs instead, so these are set explicitly: without them a `.set-line` is a
/// few pixels shorter than the webview's and the sections drift apart.
const LABEL_LINE: f32 = theme::font::BODY * 1.45;
const HELP_LINE: f32 = theme::font::DETAIL * 1.45;
/// `.set-title`: 14 px in the same line box.
const TITLE_LINE: f32 = theme::font::SECTION * 1.45;
/// A `.check input` is 15 px tall, so a row whose only control is a checkbox is
/// `14 + 18.85 + 14` tall, not the 32 px of a button row.
const CHECK_ROW_CONTROL: f32 = 19.0;
/// `input[type=range] { height: 4px }` with a 14 px thumb: the row is sized by
/// the thumb, not by a 32 px control. 10 px is the smallest height that still
/// draws the round handle without clipping it.
const SLIDER_CONTROL: f32 = 10.0;

/// `#streams-range { min="1" max="64" }`, widened to 1–128 by the brief.
const STREAMS_MIN: i64 = 1;
const STREAMS_MAX: i64 = 128;
/// `#segment option` values, widened to 8–512 MiB by the brief.
const SEGMENT_SIZES: [i64; 7] = [8, 16, 32, 64, 128, 256, 512];
const SEGMENT_MIN: i64 = 8;
const SEGMENT_MAX: i64 = 512;
/// The port the local OAuth callback listens on when settings say nothing.
const DEFAULT_REDIRECT_PORT: i64 = 8765;

/// What the screen asks the shell to do.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Action {
    /// "Sign in with Adobe".
    SignIn,
    /// "Sign out".
    SignOut,
    /// "Save" next to the Client ID (`saveSettings({ client_id })`).
    SaveClientId(String),
    /// "Copy" next to the redirect URI.
    CopyRedirect(String),
    /// "Change…" for the download folder (`PickDownloadDir`).
    ChooseDownloadFolder,
    /// The streams slider moved; the shell may debounce and save.
    Streams(i64),
    /// "Chunk size" picked a value.
    Segment(i64),
    /// "Save log with media": the value of `no_folder_logs` it writes.
    FolderLogs(bool),
    /// "Check for updates": the value of `no_update_check` it writes.
    UpdateCheck(bool),
    /// "Open at login" (`SetOpenAtLogin`).
    OpenAtLogin(bool),
    /// "Check now" / "Update" (`CheckForUpdate`, `InstallUpdate`).
    UpdateClicked,
    /// "Show in Finder" on the App log row (`OpenLogs`).
    ShowLogs,
}

/// The screen, for a shell that does not wire the actions yet.
pub fn show(ui: &mut egui::Ui, state: &mut UiState) {
    let _ = show_with_actions(ui, state);
}

/// The screen, reporting at most one [`Action`] per frame.
///
/// The sections are `index.html`'s, in its order: the Frame.io account, App,
/// Performance, Storage. Each is a `.set-title` over a `.set-card`, 12 px below
/// the title and 28 px above the next one.
pub fn show_with_actions(ui: &mut egui::Ui, state: &mut UiState) -> Option<Action> {
    let mut action = None;
    // The stylesheet stacks blocks that abut and spaces them explicitly, so the
    // shell's 8 px item spacing is zeroed before anything is drawn: the page
    // head, then `.set-title`, then `.set-card { margin-top: 12px }`.
    ui.spacing_mut().item_spacing = vec2(theme::space::M, 0.0);

    crate::app::page_head(ui, "Settings", |_ui| {});
    // No gap here: `.settings` starts straight under the 64 px `.page-head`.

    let width = ui.available_width().min(SETTINGS_MAX_WIDTH);
    ui.scope(|ui| {
        ui.set_max_width(width);

        // ---------- Frame.io account ----------
        section_title(ui, "Frame.io account");
        ui.add_space(CARD_GAP);
        card(ui, |ui| {
            sign_in_line(ui, state, &mut action);
            rule(ui);
            client_id_line(ui, state, &mut action);
            rule(ui);
            setup_line(ui, state, &mut action);
        });
        ui.add_space(SECTION_GAP);

        // ---------- App ----------
        section_title(ui, "App");
        ui.add_space(CARD_GAP);
        card(ui, |ui| {
            open_at_login_line(ui, state, &mut action);
            rule(ui);
            version_line(ui, state, &mut action);
            // `renderUpdate` hides the auto-update row in a development build.
            if !update_line(state).is_dev() {
                rule(ui);
                update_check_line(ui, state, &mut action);
            }
        });
        ui.add_space(SECTION_GAP);

        // ---------- Performance ----------
        section_title(ui, "Performance");
        ui.add_space(CARD_GAP);
        card(ui, |ui| {
            streams_line(ui, state, &mut action);
            rule(ui);
            segment_line(ui, state, &mut action);
        });
        ui.add_space(SECTION_GAP);

        // ---------- Storage ----------
        section_title(ui, "Storage");
        ui.add_space(CARD_GAP);
        card(ui, |ui| {
            download_dir_line(ui, state, &mut action);
            rule(ui);
            folder_logs_line(ui, state, &mut action);
            rule(ui);
            log_line(ui, state, &mut action);
        });
        ui.add_space(SECTION_GAP);

        // `.about`: `FioTransfer <version>`, 12 px in `--text-4`.
        if !state.version.trim().is_empty() {
            ui.label(
                RichText::new(format!("FioTransfer {}", state.version.trim()))
                    .font(theme::detail())
                    .line_height(Some(HELP_LINE))
                    .color(theme::TEXT_4),
            );
        }
    });

    action
}

/// A stored value clamped to the slider's range, so an out-of-range setting is
/// shown at the nearest end instead of being dropped.
pub fn clamped_streams(streams: i64) -> i64 {
    streams.clamp(STREAMS_MIN, STREAMS_MAX)
}

/// A stored segment size clamped to the sizes the combo offers.
pub fn clamped_segment(segment_mb: i64) -> i64 {
    segment_mb.clamp(SEGMENT_MIN, SEGMENT_MAX)
}

/// The redirect URI the setup steps ask the user to register.
pub fn redirect_uri(port: i64) -> String {
    let port = if (1..=65535).contains(&port) {
        port
    } else {
        DEFAULT_REDIRECT_PORT
    };
    format!("https://localhost:{port}/callback")
}

/// `Signed in as … · …` / `Not signed in` / `Not set up`, with the button that
/// changes it (`renderState` in app.js).
fn sign_in_line(ui: &mut egui::Ui, state: &mut UiState, action: &mut Option<Action>) {
    line(ui, |ui| {
        ui.horizontal(|ui| {
            let text = sign_in_words(state);
            ui.add(theme::truncating(
                RichText::new(text)
                    .font(theme::body())
                    .color(theme::TEXT_2),
            ))
            .on_hover_text("Frame.io account");

            ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                if state.logged_in {
                    if ui.button(RichText::new("Sign out").font(theme::body())).clicked() {
                        *action = Some(Action::SignOut);
                    }
                } else {
                    let ready = !state.settings.client_id.trim().is_empty();
                    if primary_button(ui, "Sign in with Adobe", ready) {
                        *action = Some(Action::SignIn);
                    }
                }
            });
        });
    });
}

/// The account row's words.
fn sign_in_words(state: &UiState) -> String {
    if state.logged_in {
        let name = state
            .user
            .as_ref()
            .map(|user| user.name.trim())
            .filter(|name| !name.is_empty());
        let email = state
            .user
            .as_ref()
            .map(|user| user.email.trim())
            .filter(|email| !email.is_empty());
        match (name, email) {
            (Some(name), Some(email)) => format!("Signed in as {name} · {email}"),
            (Some(name), None) => format!("Signed in as {name}"),
            (None, Some(email)) => format!("Signed in · {email}"),
            (None, None) => "Signed in".to_string(),
        }
    } else if state.settings.client_id.trim().is_empty() {
        "Not set up".to_string()
    } else {
        "Not signed in".to_string()
    }
}

/// `.set-line.col`: the label, then the field and its "Save".
fn client_id_line(ui: &mut egui::Ui, state: &mut UiState, action: &mut Option<Action>) {
    line(ui, |ui| {
        field_label(ui, "Adobe Client ID");
        ui.add_space(theme::space::M);
        ui.horizontal(|ui| {
            // Right to left, so "Save" sits on the card's edge and the field
            // takes what is left (`.set-line .inline { display: flex; gap: 8px }`).
            ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                let save = ui
                    .add(
                        egui::Button::new(RichText::new("Save").font(theme::body()))
                            .min_size(vec2(0.0, theme::size::CONTROL_H)),
                    )
                    .clicked();
                // The field's own padding is inside its width: `input` has 10 px
                // each side.
                let field_width =
                    (ui.available_width() - 2.0 * theme::space::L).max(60.0);
                ui.scope(|ui| {
                    ui.style_mut().visuals.extreme_bg_color = theme::SURFACE;
                    ui.add(
                        TextEdit::singleline(&mut state.settings.client_id)
                            .hint_text("Client ID (API key)")
                            .margin(Margin::symmetric(theme::space::L as i8, theme::space::M as i8))
                            .desired_width(field_width),
                    );
                });
                if save {
                    *action =
                        Some(Action::SaveClientId(state.settings.client_id.trim().to_string()));
                }
            });
        });
    });
}

/// The `<details class="setup">` block: five steps, the redirect URI, "Copy".
fn setup_line(ui: &mut egui::Ui, state: &mut UiState, action: &mut Option<Action>) {
    let uri = redirect_uri(state.settings.redirect_port);
    line_with(ui, CHECK_ROW_CONTROL, |ui| {
        // `.setup summary` is a line of text with a chevron, not a button.
        ui.spacing_mut().button_padding = vec2(theme::space::XL, 0.0);
        let open_by_default = state.settings.client_id.trim().is_empty();
        CollapsingHeader::new(
            RichText::new("Setup")
                .font(theme::body())
                .color(theme::TEXT_2)
                .strong(),
        )
        .id_salt("fiotransfer-egui.setup")
        .default_open(open_by_default)
        .show(ui, |ui| {
            // `.setup ol { margin: 10px 0 0 }`, `li { margin: 5px 0 }`.
            ui.add_space(theme::space::S);
            ui.spacing_mut().item_spacing.y = 5.0;
            step(ui, "1", "Adobe Developer Console → Create project");
            step(
                ui,
                "2",
                "Add API → Frame.io API → User Authentication → OAuth Single Page App",
            );
            ui.horizontal_wrapped(|ui| {
                ui.spacing_mut().item_spacing.x = theme::space::S;
                ui.label(
                    RichText::new("3")
                        .font(theme::note())
                        .color(theme::TEXT_3),
                );
                ui.label(
                    RichText::new("Default redirect URI and Redirect URI pattern:")
                        .font(theme::note())
                        .color(theme::TEXT_2),
                );
                // `code { background: var(--raised) }`
                Frame::new()
                    .fill(theme::RAISED)
                    .corner_radius(CornerRadius::same(4))
                    .inner_margin(Margin::symmetric(theme::space::S as i8, 1))
                    .show(ui, |ui| {
                        ui.label(
                            RichText::new(&uri)
                                .font(theme::mono())
                                .color(theme::TEXT),
                        );
                    });
                if link_button(ui, "Copy") {
                    *action = Some(Action::CopyRedirect(uri.clone()));
                }
            });
            step(ui, "4", "Copy the Client ID above");
            step(
                ui,
                "5",
                "On first sign-in, macOS asks for your password once to trust FioTransfer's local certificate",
            );
        });
    });
}

fn step(ui: &mut egui::Ui, number: &str, text: &str) {
    ui.horizontal_wrapped(|ui| {
        ui.spacing_mut().item_spacing.x = theme::space::S;
        ui.label(
            RichText::new(number)
                .font(theme::note())
                .color(theme::TEXT_3),
        );
        ui.add(theme::wrapping(
            RichText::new(text).font(theme::note()).color(theme::TEXT_2),
        ));
    });
}

/// `.set-line.col` with the slider's value on the right of its label.
fn streams_line(ui: &mut egui::Ui, state: &mut UiState, action: &mut Option<Action>) {
    let mut streams = clamped_streams(state.settings.streams);
    line_with(ui, CHECK_ROW_CONTROL, |ui| {
        ui.horizontal(|ui| {
            field_label(ui, "Parallel streams");
            ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                ui.label(
                    RichText::new(streams.to_string())
                        .font(theme::body())
                        .color(theme::TEXT)
                        .strong(),
                );
            });
        });
        ui.add_space(theme::space::XS);
        ui.spacing_mut().interact_size.y = SLIDER_CONTROL;
        let response = ui.add(
            Slider::new(&mut streams, STREAMS_MIN..=STREAMS_MAX)
                .show_value(false)
                .trailing_fill(true),
        );
        if response.changed() {
            *action = Some(Action::Streams(streams));
        }
    });
    state.settings.streams = streams;
}

/// "Chunk size": the select the webview draws, as a combo box.
fn segment_line(ui: &mut egui::Ui, state: &mut UiState, action: &mut Option<Action>) {
    let mut segment = clamped_segment(state.settings.segment_mb);
    line(ui, |ui| {
        ui.horizontal(|ui| {
            let label = field_label(ui, "Chunk size");
            ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                let before = segment;
                let combo = ComboBox::from_id_salt("fiotransfer-egui.segment")
                    .selected_text(RichText::new(format!("{segment} MB")).font(theme::body()))
                    .width(120.0)
                    .show_ui(ui, |ui| {
                        for mb in SEGMENT_SIZES {
                            ui.selectable_value(&mut segment, mb, format!("{mb} MB"));
                        }
                    });
                // `select` is associated with its `label` in the webview too.
                combo.response.labelled_by(label.id);
                if segment != before {
                    *action = Some(Action::Segment(segment));
                }
            });
        });
    });
    state.settings.segment_mb = segment;
}

/// `.set-line` with two lines on the left: the label and the path.
fn download_dir_line(ui: &mut egui::Ui, state: &mut UiState, action: &mut Option<Action>) {
    line(ui, |ui| {
        ui.horizontal(|ui| {
            ui.vertical(|ui| {
                ui.set_width((ui.available_width() - 100.0).max(80.0));
                ui.spacing_mut().interact_size.y = LABEL_LINE;
                field_label(ui, "Download location");
                ui.add_space(theme::space::XXS);
                ui.add(theme::truncating(
                    RichText::new(display_path(&state.settings.download_dir, &state.home))
                        .font(theme::detail())
                        .line_height(Some(HELP_LINE))
                        .color(theme::TEXT_3),
                ))
                .on_hover_text(if state.settings.download_dir.trim().is_empty() {
                    "No download folder chosen".to_string()
                } else {
                    state.settings.download_dir.clone()
                });
            });
            ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                if ui.button(RichText::new("Change…").font(theme::body())).clicked() {
                    *action = Some(Action::ChooseDownloadFolder);
                }
            });
        });
    });
}

/// "Save log with media": the label on the left, the box on the right.
fn folder_logs_line(ui: &mut egui::Ui, state: &mut UiState, action: &mut Option<Action>) {
    let mut save_logs = !state.settings.no_folder_logs;
    line_with(ui, CHECK_ROW_CONTROL, |ui| {
        ui.horizontal(|ui| {
            let label = field_label(ui, "Save log with media");
            ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                ui.spacing_mut().interact_size.y = CHECK_ROW_CONTROL;
                let boxed = ui.add(Checkbox::without_text(&mut save_logs)).labelled_by(label.id);
                if boxed.changed() {
                    *action = Some(Action::FolderLogs(save_logs));
                }
            });
        });
    });
    state.settings.no_folder_logs = !save_logs;
}

/// "Check for updates": the same row, reading `no_update_check`.
fn update_check_line(ui: &mut egui::Ui, state: &mut UiState, action: &mut Option<Action>) {
    let mut check = !state.settings.no_update_check;
    line_with(ui, CHECK_ROW_CONTROL, |ui| {
        ui.horizontal(|ui| {
            let label = field_label(ui, "Check for updates");
            ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                ui.spacing_mut().interact_size.y = CHECK_ROW_CONTROL;
                let boxed = ui.add(Checkbox::without_text(&mut check)).labelled_by(label.id);
                if boxed.changed() {
                    *action = Some(Action::UpdateCheck(check));
                }
            });
        });
    });
    state.settings.no_update_check = !check;
}


/// `UpdateLine` from `renderUpdate` in app.js: the help text under "Version",
/// the button's word, and whether the button is live.
#[derive(Debug, Clone, PartialEq, Eq)]
struct UpdateLine {
    help: String,
    error: String,
    button: &'static str,
    button_enabled: bool,
    button_primary: bool,
    dev: bool,
}

impl UpdateLine {
    fn is_dev(&self) -> bool {
        self.dev
    }
}

/// What `renderUpdate` would write into `#update-line` and `#update-btn`.
fn update_line(state: &UiState) -> UpdateLine {
    let update = &state.update;
    let dev = update.blocker.trim() == "development build";
    let busy = matches!(update.state.as_str(), "downloading" | "restarting");

    let help = if dev {
        "Development build".to_string()
    } else if update.state == "downloading" {
        format!("Downloading… {}%", (update.progress.clamp(0.0, 1.0) * 100.0).floor())
    } else if update.state == "restarting" {
        "Restarting…".to_string()
    } else if update.state == "checking" {
        "Checking…".to_string()
    } else if update.available {
        let mut help = format!("{} available", update.latest.trim());
        if !update.blocker.trim().is_empty() {
            help.push_str(" · ");
            help.push_str(update.blocker.trim());
        }
        help
    } else {
        // `#update-line` is only filled once a check has answered; `checked_at`
        // is not in the state, so an unchecked build shows nothing here.
        String::new()
    };

    UpdateLine {
        help,
        error: update.error.trim().to_string(),
        button: if update.available {
            if update.blocker.trim().is_empty() {
                "Update"
            } else {
                "Download"
            }
        } else {
            "Check now"
        },
        button_enabled: !dev && !busy && update.state != "checking",
        button_primary: update.available && update.blocker.trim().is_empty(),
        dev,
    }
}

/// "Open at login": the label on the left, the box on the right.
fn open_at_login_line(ui: &mut egui::Ui, state: &mut UiState, action: &mut Option<Action>) {
    let mut open_at_login = state.open_at_login;
    line_with(ui, CHECK_ROW_CONTROL, |ui| {
        ui.horizontal(|ui| {
            let label = field_label(ui, "Open at login");
            ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                ui.spacing_mut().interact_size.y = CHECK_ROW_CONTROL;
                let boxed = ui
                    .add(Checkbox::without_text(&mut open_at_login))
                    .labelled_by(label.id);
                if boxed.changed() {
                    *action = Some(Action::OpenAtLogin(open_at_login));
                }
            });
        });
    });
    state.open_at_login = open_at_login;
}

/// "Version <current>" with the update line beneath and the button on the right.
fn version_line(ui: &mut egui::Ui, state: &mut UiState, action: &mut Option<Action>) {
    let current = update_current(state);
    let words = update_line(state);
    line(ui, |ui| {
        ui.horizontal(|ui| {
            ui.vertical(|ui| {
                ui.set_width((ui.available_width() - 130.0).max(80.0));
                ui.spacing_mut().interact_size.y = LABEL_LINE;
                ui.horizontal(|ui| {
                    ui.spacing_mut().item_spacing.x = theme::space::XS;
                    field_label(ui, "Version");
                    // `#app-version { color: var(--text-2); font-weight: 400 }`
                    ui.label(
                        RichText::new(&current)
                            .font(theme::body())
                            .color(theme::TEXT_2),
                    );
                });
                if !words.help.is_empty() {
                    ui.add_space(theme::space::XXS);
                    ui.add(theme::truncating(
                        RichText::new(&words.help)
                            .font(theme::detail())
                            .line_height(Some(HELP_LINE))
                            .color(theme::TEXT_3),
                    ))
                    .on_hover_text(&words.help);
                }
                if !words.error.is_empty() {
                    ui.add_space(theme::space::XXS);
                    ui.add(theme::truncating(
                        RichText::new(&words.error)
                            .font(theme::detail())
                            .line_height(Some(HELP_LINE))
                            .color(theme::ERROR_TEXT),
                    ))
                    .on_hover_text(&words.error);
                }
            });
            // `btn.classList.toggle('hidden', dev)`: a development build has no
            // button at all.
            if !words.is_dev() {
                ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                    let clicked = if words.button_primary {
                        primary_button(ui, words.button, words.button_enabled)
                    } else {
                        secondary_button_enabled(ui, words.button, words.button_enabled)
                    };
                    if clicked {
                        *action = Some(Action::UpdateClicked);
                    }
                });
            }
        });
    });
}

/// `u.current`, falling back to the screen's own version.
fn update_current(state: &UiState) -> String {
    let current = state.update.current.trim();
    if current.is_empty() {
        state.version.trim().to_string()
    } else {
        current.to_string()
    }
}

/// "App log": the label, the shortened log path, and "Show in Finder".
fn log_line(ui: &mut egui::Ui, state: &mut UiState, action: &mut Option<Action>) {
    let path = display_path(&state.log_dir, &state.home);
    let full = state.log_dir.trim().to_string();
    line(ui, |ui| {
        ui.horizontal(|ui| {
            ui.vertical(|ui| {
                ui.set_width((ui.available_width() - 140.0).max(80.0));
                ui.spacing_mut().interact_size.y = LABEL_LINE;
                field_label(ui, "App log");
                ui.add_space(theme::space::XXS);
                ui.add(theme::truncating(
                    RichText::new(&path)
                        .font(theme::detail())
                        .line_height(Some(HELP_LINE))
                        .color(theme::TEXT_3),
                ))
                .on_hover_text(if full.is_empty() {
                    "No log folder yet".to_string()
                } else {
                    full.clone()
                });
            });
            ui.with_layout(Layout::right_to_left(Align::Center), |ui| {
                if ui
                    .button(RichText::new("Show in Finder").font(theme::body()))
                    .clicked()
                {
                    *action = Some(Action::ShowLogs);
                }
            });
        });
    });
}

/// The path as the webview shows it, or an em dash when nothing is set.
pub fn display_path(path: &str, home: &str) -> String {
    let trimmed = path.trim();
    if trimmed.is_empty() {
        "—".to_string()
    } else {
        short_path(trimmed, home)
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

// ---------- the pieces of the layout ----------

/// `.set-title`: 14 px, 600, `--text`.
fn section_title(ui: &mut egui::Ui, title: &str) {
    ui.label(
        RichText::new(title)
            .font(theme::section())
            .line_height(Some(TITLE_LINE))
            .color(theme::TEXT)
            .strong(),
    );
}

/// `.set-line label`: 13 px, 500, `--text`.
fn field_label(ui: &mut egui::Ui, text: &str) -> egui::Response {
    ui.label(
        RichText::new(text)
            .font(theme::body())
            .line_height(Some(LABEL_LINE))
            .color(theme::TEXT)
            .strong(),
    )
}

/// `.set-card`: `--panel`, 1 px `--border`, 12 px radius, no padding — the
/// lines pad themselves so the separators reach the card's edges.
fn card(ui: &mut egui::Ui, add: impl FnOnce(&mut egui::Ui)) {
    theme::panel_card_frame().show(ui, |ui| {
        // `.settings { max-width: 680px }` binds the card, and the lines inside
        // take the card's width so every right-aligned control ends on the same
        // edge.
        ui.set_max_width(ui.available_width().min(SETTINGS_MAX_WIDTH));
        ui.set_width(ui.available_width());
        add(ui);
    });
}

/// `.set-line + .set-line { border-top: 1px solid var(--border) }`: a hairline
/// that reaches both edges of the card, with no gap around it.
fn rule(ui: &mut egui::Ui) {
    let width = ui.available_width();
    let (rect, _) = ui.allocate_exact_size(vec2(width, 1.0), egui::Sense::hover());
    ui.painter()
        .hline(rect.x_range(), rect.center().y, theme::hairline(theme::BORDER));
}

/// `.set-line { padding: 14px 16px }`.
fn line(ui: &mut egui::Ui, add: impl FnOnce(&mut egui::Ui)) {
    line_with(ui, theme::size::CONTROL_H, add);
}

/// `.set-line` whose tallest control is `control` px tall.
///
/// `Ui::horizontal` gives its row the height of `spacing.interact_size.y`, so a
/// row whose only control is a 15 px checkbox has to be told it is 19 px tall —
/// otherwise every toggle would be a 32 px button row.
fn line_with(ui: &mut egui::Ui, control: f32, add: impl FnOnce(&mut egui::Ui)) {
    Frame::new().inner_margin(LINE_PADDING).show(ui, |ui| {
        ui.set_width(ui.available_width());
        ui.spacing_mut().interact_size.y = control;
        add(ui);
    });
}

/// `.btn.primary`: `--brand` fill, white text, 32 px tall. `enabled == false`
/// draws it at 40 %, like `.btn:disabled { opacity: .4 }`.
fn primary_button(ui: &mut egui::Ui, text: &str, enabled: bool) -> bool {
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
            .add_enabled(
                enabled,
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

/// `.btn` that can be disabled (`.btn:disabled { opacity: .4 }`).
fn secondary_button_enabled(ui: &mut egui::Ui, text: &str, enabled: bool) -> bool {
    let mut clicked = false;
    ui.scope(|ui| {
        {
            let widgets = &mut ui.style_mut().visuals.widgets;
            widgets.inactive.weak_bg_fill = theme::CARD;
            widgets.hovered.weak_bg_fill = theme::RAISED;
            widgets.active.weak_bg_fill = theme::RAISED;
            widgets.inactive.bg_stroke = theme::stroke_border_2();
            widgets.hovered.bg_stroke = theme::hairline(theme::BORDER_3);
            widgets.inactive.fg_stroke = theme::hairline(theme::TEXT_2);
            widgets.hovered.fg_stroke = theme::hairline(theme::TEXT);
            widgets.inactive.corner_radius = theme::R_S;
            widgets.hovered.corner_radius = theme::R_S;
            widgets.active.corner_radius = theme::R_S;
        }
        clicked = ui
            .add_enabled(
                enabled,
                egui::Button::new(RichText::new(text).font(theme::body()).color(theme::TEXT_2))
                    .min_size(vec2(0.0, theme::size::CONTROL_H)),
            )
            .clicked();
    });
    clicked
}

/// `a` in the stylesheet: 12.5 px, `#9a95ff`, lighter on hover.
fn link_button(ui: &mut egui::Ui, text: &str) -> bool {
    let mut clicked = false;
    ui.scope(|ui| {
        {
            let widgets = &mut ui.style_mut().visuals.widgets;
            widgets.inactive.weak_bg_fill = Color32::TRANSPARENT;
            widgets.hovered.weak_bg_fill = Color32::TRANSPARENT;
            widgets.active.weak_bg_fill = Color32::TRANSPARENT;
            widgets.inactive.bg_stroke = Stroke::NONE;
            widgets.hovered.bg_stroke = Stroke::NONE;
            widgets.active.bg_stroke = Stroke::NONE;
            widgets.inactive.fg_stroke = theme::hairline(theme::LINK);
            widgets.hovered.fg_stroke = theme::hairline(theme::LINK_HOVER);
            widgets.active.fg_stroke = theme::hairline(theme::LINK);
        }
        clicked = ui
            .add(
                egui::Button::new(
                    RichText::new(text)
                        .font(theme::note())
                        .color(theme::LINK),
                )
                .min_size(vec2(0.0, 22.0)),
            )
            .clicked();
    });
    clicked
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::{Settings, UpdateState, User};
    use egui_kittest::Harness;
    use egui_kittest::kittest::Queryable;

    fn state(settings: Settings, logged_in: bool) -> UiState {
        UiState {
            logged_in,
            settings,
            version: "0.9.3".to_string(),
            open_at_login: false,
            log_dir: "/Users/chaos/Library/Logs/FioTransfer".to_string(),
            home: "/Users/chaos".to_string(),
            ..UiState::default()
        }
    }

    fn configured() -> Settings {
        Settings {
            client_id: String::new(),
            redirect_port: 8765,
            streams: 16,
            segment_mb: 32,
            download_dir: "/Users/chaos/Downloads".to_string(),
            no_folder_logs: false,
            no_update_check: false,
            last_folder_label: String::new(),
        }
    }

    fn screen_harness(state: UiState) -> Harness<'static, UiState> {
        Harness::builder()
            .with_size(vec2(956.0, 2400.0))
            .build_state(
                |ctx, state: &mut UiState| {
                    crate::app::apply_style_once(ctx);
                    egui::CentralPanel::default().show(ctx, |ui| show(ui, state));
                },
                state,
            )
    }

    /// The toggle boxes in draw order, which is the order of the rows: "Open at
    /// login" and "Check for updates" (App), then "Save log with media"
    /// (Storage). Each box's text label is drawn to its left, the way
    /// `.set-line` puts a `label` beside a `.check` input.
    fn toggle_boxes<'a>(harness: &'a Harness<'a, UiState>) -> Vec<egui_kittest::Node<'a>> {
        harness
            .get_all_by_role(egui::accesskit::Role::CheckBox)
            .collect()
    }

    #[test]
    fn signed_out_settings_shows_the_client_id_setup_and_every_section() {
        let harness = screen_harness(state(configured(), false));

        // `index.html`'s titles, in its order: account, App, Performance, Storage.
        for heading in ["Settings", "Frame.io account", "App", "Performance", "Storage"] {
            harness.get_by_label(heading);
        }
        harness.get_by_label("Not set up");
        harness.get_by_label("Sign in with Adobe");
        harness.get_by_label("Adobe Client ID");
        harness.get_by_label("Save");
        harness.get_by_label("Setup");
        harness.get_by_label("Copy");
        harness.get_by_label("Parallel streams");
        harness.get_by_label("16");
        harness.get_by_label("Chunk size");
    }

    #[test]
    fn the_app_card_has_the_login_version_and_update_rows() {
        let harness = screen_harness(state(configured(), true));

        harness.get_by_label("Open at login");
        harness.get_by_label("Version");
        harness.get_by_label("0.9.3");
        harness.get_by_label("Check now");
        harness.get_by_label("Check for updates");
    }

    #[test]
    fn the_storage_card_has_the_folder_the_log_toggle_and_the_app_log() {
        let harness = screen_harness(state(configured(), true));

        harness.get_by_label("Download location");
        harness.get_by_label("~/Downloads");
        harness.get_by_label("Change…");
        harness.get_by_label("Save log with media");
        harness.get_by_label("App log");
        harness.get_by_label("~/Library/Logs/FioTransfer");
        harness.get_by_label("Show in Finder");
    }

    #[test]
    fn the_about_line_names_the_build() {
        let harness = screen_harness(state(configured(), true));
        harness.get_by_label("FioTransfer 0.9.3");
    }

    #[test]
    fn the_version_number_falls_back_to_the_screen_version() {
        let mut ui_state = state(configured(), true);
        ui_state.update.current = "9.9.9".to_string();
        assert_eq!(update_current(&ui_state), "9.9.9");
        screen_harness(ui_state).get_by_label("9.9.9");

        let mut ui_state = state(configured(), true);
        ui_state.update.current = String::new();
        assert_eq!(update_current(&ui_state), "0.9.3");
        screen_harness(ui_state).get_by_label("0.9.3");
    }

    #[test]
    fn the_update_line_speaks_the_webview_words() {
        let mut ui_state = state(configured(), true);

        // Nothing checked yet: no help text, and the button offers a check.
        let idle = update_line(&ui_state);
        assert_eq!(idle.help, "");
        assert_eq!(idle.button, "Check now");
        assert!(idle.button_enabled);

        ui_state.update = UpdateState {
            current: "0.9.3".to_string(),
            state: "checking".to_string(),
            ..UpdateState::default()
        };
        let checking = update_line(&ui_state);
        assert_eq!(checking.help, "Checking…");
        assert!(!checking.button_enabled, "a check in flight disables the button");

        ui_state.update = UpdateState {
            latest: "0.9.5".to_string(),
            available: true,
            state: "idle".to_string(),
            ..UpdateState::default()
        };
        let available = update_line(&ui_state);
        assert_eq!(available.help, "0.9.5 available");
        assert_eq!(available.button, "Update");
        assert!(available.button_primary);

        ui_state.update.blocker = "download only".to_string();
        let blocked = update_line(&ui_state);
        assert_eq!(blocked.help, "0.9.5 available · download only");
        assert_eq!(blocked.button, "Download");
        assert!(!blocked.button_primary);

        ui_state.update = UpdateState {
            state: "downloading".to_string(),
            progress: 0.42,
            ..UpdateState::default()
        };
        let downloading = update_line(&ui_state);
        assert_eq!(downloading.help, "Downloading… 42%");
        assert!(!downloading.button_enabled);

        ui_state.update = UpdateState {
            blocker: "development build".to_string(),
            ..UpdateState::default()
        };
        let dev = update_line(&ui_state);
        assert_eq!(dev.help, "Development build");
        assert!(dev.is_dev());
        assert!(!dev.button_enabled, "a development build cannot update itself");
    }

    #[test]
    fn a_development_build_hides_the_update_toggle() {
        let mut ui_state = state(configured(), true);
        ui_state.update.blocker = "development build".to_string();
        let harness = screen_harness(ui_state);

        harness.get_by_label("Development build");
        assert!(
            harness.query_by_label("Check for updates").is_none(),
            "`renderUpdate` hides the auto-update row in a development build"
        );
        assert!(harness.query_by_label("Check now").is_none());
    }

    #[test]
    fn the_update_error_is_shown_under_the_version() {
        let mut ui_state = state(configured(), true);
        ui_state.update.error = "Could not reach the release page".to_string();
        let harness = screen_harness(ui_state);
        harness.get_by_label("Could not reach the release page");
    }

    #[test]
    fn signed_in_settings_offers_sign_out_and_names_the_user() {
        let mut settings = Settings::default();
        settings.client_id = "abc123".to_string();
        let mut ui_state = state(settings, true);
        ui_state.user = Some(User {
            name: "Ada Lovelace".to_string(),
            email: "ada@example.com".to_string(),
        });
        let harness = screen_harness(ui_state);

        harness.get_by_label("Signed in as Ada Lovelace · ada@example.com");
        harness.get_by_label("Sign out");
        assert!(harness.query_by_label("Sign in with Adobe").is_none());
    }

    #[test]
    fn a_client_id_turns_not_set_up_into_not_signed_in() {
        let mut settings = Settings::default();
        settings.client_id = "abc123".to_string();
        let harness = screen_harness(state(settings, false));

        harness.get_by_label("Not signed in");
        harness.get_by_label("Sign in with Adobe");
    }

    #[test]
    fn a_signed_in_user_without_a_name_still_reads_sensibly() {
        let mut ui_state = state(Settings::default(), true);
        ui_state.user = None;
        assert_eq!(sign_in_words(&ui_state), "Signed in");

        ui_state.user = Some(User {
            name: "  ".to_string(),
            email: "ada@example.com".to_string(),
        });
        assert_eq!(sign_in_words(&ui_state), "Signed in · ada@example.com");

        ui_state.user = Some(User {
            name: "Ada".to_string(),
            email: String::new(),
        });
        assert_eq!(sign_in_words(&ui_state), "Signed in as Ada");
    }

    #[test]
    fn the_three_toggles_write_their_settings() {
        let mut harness = screen_harness(state(Settings::default(), true));
        assert!(!harness.state().open_at_login);
        assert!(!harness.state().settings.no_update_check);
        assert!(!harness.state().settings.no_folder_logs);
        assert_eq!(toggle_boxes(&harness).len(), 3, "three toggle rows");

        // Open at login.
        {
            let boxes = toggle_boxes(&harness);
            if let Some(boxed) = boxes.first() {
                boxed.click();
            }
        }
        harness.run();
        assert!(harness.state().open_at_login);

        // Check for updates.
        {
            let boxes = toggle_boxes(&harness);
            if let Some(boxed) = boxes.get(1) {
                boxed.click();
            }
        }
        harness.run();
        assert!(harness.state().settings.no_update_check);

        // Save log with media.
        {
            let boxes = toggle_boxes(&harness);
            if let Some(boxed) = boxes.get(2) {
                boxed.click();
            }
        }
        harness.run();
        assert!(harness.state().settings.no_folder_logs);
    }

    #[test]
    fn the_streams_value_follows_the_slider_and_stays_in_range() {
        let mut settings = Settings::default();
        settings.streams = 16;
        let harness = screen_harness(state(settings, true));
        harness.get_by_label("16");

        // A stored value outside 1–128 is shown clamped, never dropped.
        assert_eq!(clamped_streams(0), STREAMS_MIN);
        assert_eq!(clamped_streams(999), STREAMS_MAX);
        assert_eq!(clamped_streams(64), 64);

        let mut settings = Settings::default();
        settings.streams = 999;
        let clamped = screen_harness(state(settings, true));
        clamped.get_by_label("128");
        assert_eq!(clamped.state().settings.streams, STREAMS_MAX);
    }

    #[test]
    fn a_segment_size_outside_the_range_is_clamped_not_dropped() {
        assert_eq!(clamped_segment(0), SEGMENT_MIN);
        assert_eq!(clamped_segment(4096), SEGMENT_MAX);
        assert_eq!(clamped_segment(256), 256);

        let mut settings = Settings::default();
        settings.segment_mb = 4096;
        let harness = screen_harness(state(settings, true));
        harness.get_by_label("Chunk size");
        assert_eq!(harness.state().settings.segment_mb, SEGMENT_MAX);
    }

    #[test]
    fn the_redirect_uri_is_built_from_the_port_and_falls_back() {
        assert_eq!(redirect_uri(8765), "https://localhost:8765/callback");
        assert_eq!(redirect_uri(0), "https://localhost:8765/callback");
        assert_eq!(redirect_uri(-4), "https://localhost:8765/callback");
        assert_eq!(redirect_uri(70000), "https://localhost:8765/callback");
        assert_eq!(redirect_uri(9000), "https://localhost:9000/callback");
        assert_eq!(display_path("", "/Users/chaos"), "—");
        assert_eq!(
            display_path("  /Users/chaos/Downloads ", "/Users/chaos"),
            "~/Downloads"
        );
        assert_eq!(display_path("/Volumes/Raid/x", ""), "/Volumes/Raid/x");
        assert_eq!(short_path("/Users/chaos", "/Users/chaos"), "~");
    }

    #[test]
    fn copy_reports_the_redirect_uri() {
        let seen: std::rc::Rc<std::cell::RefCell<Vec<Action>>> = Default::default();
        let sink = seen.clone();
        let mut harness = Harness::builder()
            .with_size(vec2(956.0, 2400.0))
            .build_state(
                move |ctx, state: &mut UiState| {
                    egui::CentralPanel::default().show(ctx, |ui| {
                        if let Some(action) = show_with_actions(ui, state) {
                            sink.borrow_mut().push(action);
                        }
                    });
                },
                state(Settings::default(), true),
            );

        harness.get_by_label("Copy").click();
        harness.run();

        assert!(
            seen.borrow()
                .contains(&Action::CopyRedirect(redirect_uri(8765))),
            "actions seen: {:?}",
            seen.borrow()
        );
    }

    #[test]
    fn show_in_finder_and_check_now_report_their_actions() {
        let seen: std::rc::Rc<std::cell::RefCell<Vec<Action>>> = Default::default();
        let sink = seen.clone();
        let mut harness = Harness::builder()
            .with_size(vec2(956.0, 2400.0))
            .build_state(
                move |ctx, state: &mut UiState| {
                    egui::CentralPanel::default().show(ctx, |ui| {
                        if let Some(action) = show_with_actions(ui, state) {
                            sink.borrow_mut().push(action);
                        }
                    });
                },
                state(Settings::default(), true),
            );

        harness.get_by_label("Check now").click();
        harness.run();
        assert!(seen.borrow().contains(&Action::UpdateClicked), "{:?}", seen.borrow());

        harness.get_by_label("Show in Finder").click();
        harness.run();
        assert!(seen.borrow().contains(&Action::ShowLogs), "{:?}", seen.borrow());
    }

    #[test]
    fn an_empty_folder_and_log_shows_an_em_dash_for_each() {
        let mut ui_state = state(Settings::default(), true);
        ui_state.log_dir = String::new();
        let harness = screen_harness(ui_state);

        harness.get_by_label("Download location");
        harness.get_by_label("App log");
        assert_eq!(
            harness.get_all_by_label("—").count(),
            2,
            "both the folder and the log path are unset"
        );
    }

    #[test]
    fn a_very_long_path_is_truncated_not_wrapped() {
        let long = format!("/Users/chaos/{}", "deep-folder/".repeat(60));
        let mut settings = Settings::default();
        settings.download_dir = long.clone();
        let harness = screen_harness(state(settings, true));
        // The label carries the whole path, shortened to `~` by `shortPath`.
        harness.get_by_label(short_path(&long, "/Users/chaos").as_str());
    }
}
