//! The reconciler: Portal's `.turbosort` markers become mirrored watch folders.
//!
//! Portal publishes upload/download links and writes a `.turbosort` marker into
//! a link's Frame.io folder. The marker names the `Live_Work` folder the project
//! belongs to. For every link that carries one, this module keeps exactly one
//! watch folder: local `portal_root/<marker>` <-> the link's Frame.io folder,
//! both ways (`mirror`).
//!
//! What it never does:
//!
//! * touch Portal (only `GET /api/upload-links` is called; no file goes there);
//! * adopt Portal's `destination_path` / `path_template` — the local root is a
//!   FioTransfer setting, and the folder name is the marker, one path segment;
//! * touch a watch the user made. Only a watch whose `origin` is
//!   `portal:<link id>` may be updated or removed, and only while that link is
//!   live and still asks for a folder;
//! * delete anything local. The root and the folders under it are created, and
//!   nothing else on disk is created, moved or removed.

use std::collections::{BTreeMap, BTreeSet};
use std::path::{Path, PathBuf};
use std::sync::Arc;

use serde::Serialize;

use crate::app::App;
use crate::history::watch::DIR_MIRROR;
use crate::history::watch::Watch;
use crate::portal::{self, PortalClient, UploadLink};

/// The origin a Portal-owned watch carries: `portal:<link id>`.
pub fn origin_of(link_id: &str) -> String {
    format!("portal:{}", link_id)
}

/// The link id of a Portal-owned watch, or `None` for a watch the user made.
pub fn link_of(origin: &str) -> Option<&str> {
    origin.strip_prefix("portal:")
}

/// The longest marker accepted. A single path segment on any of the three
/// platforms is 255 bytes; a longer one is not a folder name.
pub const MAX_MARKER: usize = 255;

/// One link that produced no watch, and why.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct Skip {
    pub link: String,
    pub marker: String,
    pub reason: String,
}

/// What one reconcile did.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct Report {
    /// When it ran (unix ms).
    pub at: i64,
    /// How many rows `GET /api/upload-links` answered with.
    pub links_seen: usize,
    /// Watches created or brought back in line.
    pub ensured: usize,
    /// Portal-owned watches removed (link gone, link off, marker cleared, or a
    /// duplicate of another link's folder).
    pub removed: usize,
    /// Links that could not be acted on. An empty list is the normal case.
    pub skipped: Vec<Skip>,
    /// Why nothing was done, when the whole run failed (no token, no URL,
    /// Portal unreachable, history closed). Nothing is changed when this is set.
    pub error: String,
}

/// What the front end shows: the settings, whether a token is stored, and the
/// last reconcile.
#[derive(Debug, Clone, Default, Serialize)]
pub struct PortalStatus {
    pub enabled: bool,
    /// A bearer token is stored (the token itself is never returned).
    pub signed_in: bool,
    pub url: String,
    pub root: String,
    pub running: bool,
    pub last_at: i64,
    pub links_seen: usize,
    pub ensured: usize,
    pub removed: usize,
    pub skipped: usize,
    pub error: String,
}

/// Whether a `.turbosort` value is exactly one path segment.
///
/// This is the safety check the whole feature rests on: the marker is joined to
/// the local root, so `/`, `\`, `%` (an escape that could reach another route),
/// `.`, `..`, an empty value or a control character must never get through. A
/// marker of `..` would otherwise point the watch at the root's parent, and an
/// absolute one at the whole disk.
pub fn check_marker(x: &str) -> Result<(), String> {
    if x.is_empty() {
        return Err("the turbosort marker is empty".to_string());
    }
    if x == "." || x == ".." {
        return Err(format!("the turbosort marker {:?} is not a folder name", x));
    }
    if x.len() > MAX_MARKER {
        return Err(format!("the turbosort marker is longer than {} bytes", MAX_MARKER));
    }
    if let Some(c) = x.chars().find(|c| *c == '/' || *c == '\\' || *c == '%' || c.is_control()) {
        return Err(format!("the turbosort marker {:?} contains {:?}: one path segment only", x, c));
    }
    Ok(())
}

/// One watch Portal wants.
#[derive(Debug, Clone)]
struct Target {
    link: String,
    marker: String,
    local: String,
    acct: String,
    folder: String,
    label: String,
    rule: String,
}

/// Reads Portal, then applies what it says. A failure to read changes nothing.
pub async fn reconcile(app: &App) -> Report {
    let s = app.settings();
    let mut rep = Report { at: crate::xfer::now_ms(), ..Default::default() };
    if app.hist().is_none() {
        rep.error = "history isn't open".to_string();
        return rep;
    }
    if app.wat().is_none() {
        rep.error = "watch folders aren't running".to_string();
        return rep;
    }
    let client = match PortalClient::from_settings(&s) {
        Ok(c) => c,
        Err(e) => {
            rep.error = e.to_string();
            return rep;
        }
    };
    let links = match client.upload_links().await {
        Ok(l) => l,
        Err(e) => {
            rep.error = e.to_string();
            return rep;
        }
    };
    rep.links_seen = links.len();
    apply(app, &links, &mut rep);
    rep
}

/// The local side: create the root, then add, edit and remove the watches Portal
/// asks for. Synchronous, so a caller can run it twice and compare.
fn apply(app: &App, links: &[UploadLink], rep: &mut Report) {
    let s = app.settings();
    let (Some(hist), Some(wat)) = (app.hist(), app.wat()) else {
        rep.error = "watch folders aren't running".to_string();
        return;
    };
    let root = s.portal_root_path();
    if let Err(e) = std::fs::create_dir_all(&root) {
        rep.error = format!("can't create {}: {}", root.display(), e);
        return;
    }
    let targets = plan(links, &root, &s.portal_change_rule(), rep);
    let wanted: BTreeSet<String> = targets.iter().map(|t| origin_of(&t.link)).collect();

    // First: Portal-owned watches that Portal no longer asks for. A user's watch
    // has no `portal:` origin, so it is never in this list.
    for w in hist.watches().unwrap_or_default().iter().filter(|w| link_of(&w.origin).is_some()) {
        if !wanted.contains(&w.origin) {
            wat.remove(&w.id);
            rep.removed += 1;
            hist.log(&format!("PORTAL   removed  [{}] {} (the link no longer asks for it)", w.id, w.local_path));
        }
    }

    for t in &targets {
        let origin = origin_of(&t.link);
        // Read back each time: an earlier target may have moved or removed a
        // watch, and Portal owns every one of these.
        let rows = hist.watches().unwrap_or_default();

        // A Portal watch sitting on this folder under another link's origin is
        // stale (its own link wants some other folder, or is gone): Portal owns
        // it, so it goes. Its link's own turn — before or after this one —
        // makes the watch that is right for it.
        if let Some(other) = rows
            .iter()
            .find(|w| w.local_path == t.local && w.origin != origin && link_of(&w.origin).is_some())
        {
            wat.remove(&other.id);
            rep.removed += 1;
            hist.log(&format!("PORTAL   moved    [{}] {} (taken over by {})", other.id, other.local_path, t.link));
        }
        // Several rows for one link cannot happen; if one is there, the extras go.
        let mine: Vec<Watch> = rows.iter().filter(|w| w.origin == origin).cloned().collect();
        for extra in mine.iter().skip(1) {
            wat.remove(&extra.id);
            rep.removed += 1;
        }

        // The watch has to have a folder to point at; `add` and `update` both
        // refuse one that isn't there. Only a directory under the root is ever
        // created here, never a file, and nothing local is ever removed.
        if let Err(e) = std::fs::create_dir_all(&t.local) {
            rep.skipped.push(Skip {
                link: t.link.clone(),
                marker: t.marker.clone(),
                reason: format!("can't create {}: {}", t.local, e),
            });
            continue;
        }

        match mine.first() {
            Some(w) => {
                if matches(w, t) {
                    continue; // already right: a second run changes nothing
                }
                match wat.update(&w.id, &t.local, &t.acct, &t.folder, &t.label, DIR_MIRROR, &t.rule, false) {
                    Ok(()) => {
                        rep.ensured += 1;
                        hist.log(&format!("PORTAL   edited   [{}] {} <-> {} ({})", w.id, t.local, t.label, t.marker));
                    }
                    Err(e) => rep.skipped.push(Skip {
                        link: t.link.clone(),
                        marker: t.marker.clone(),
                        reason: e,
                    }),
                }
            }
            None => {
                // A watch the user made in that folder is untouchable, whatever
                // Portal says the folder should hold.
                if rows.iter().any(|w| w.local_path == t.local && link_of(&w.origin).is_none()) {
                    rep.skipped.push(Skip {
                        link: t.link.clone(),
                        marker: t.marker.clone(),
                        reason: format!("{} is already watched by a watch you made", t.local),
                    });
                    continue;
                }
                match wat.add(&t.local, &t.acct, &t.folder, &t.label, DIR_MIRROR, &t.rule, false) {
                    Ok(()) => {
                        // The watch is the watcher's now; it has to carry the
                        // origin that says Portal may take it away again.
                        adopt_origin(&hist, &wat, &t.local, &origin);
                        rep.ensured += 1;
                        hist.log(&format!("PORTAL   added    {} <-> {} ({})", t.local, t.label, t.marker));
                    }
                    Err(e) => rep.skipped.push(Skip {
                        link: t.link.clone(),
                        marker: t.marker.clone(),
                        reason: e,
                    }),
                }
            }
        }
    }
}

/// Stamps the watch just made in `local` with `origin`, so the next reconcile
/// finds it instead of making a second one and the one after that may remove it.
/// `Watcher::add` does not take an origin (a user's watch has none), so it is
/// recorded after the fact, through the watcher, which keeps its own copy.
/// A failure is logged rather than ignored: without the origin the next
/// reconcile would read the watch as the user's own and leave it alone.
fn adopt_origin(hist: &crate::history::Store, wat: &crate::xfer::watch::Watcher, local: &str, origin: &str) {
    let done = hist
        .watches()
        .map(|rows| {
            rows.iter()
                .find(|w| w.local_path == local && w.origin.is_empty())
                .map(|w| wat.set_origin(&w.id, origin))
                .is_some()
        })
        .unwrap_or(false);
    if !done {
        hist.log(&format!("PORTAL   could not record {} as Portal's watch for {}", local, origin));
    }
}

/// Whether a watch already is what Portal wants for this link.
///
/// `enabled` is deliberately not compared: a user who paused a Portal watch
/// meant it, and a reconcile that re-enabled it every 30 s would undo that.
fn matches(w: &Watch, t: &Target) -> bool {
    w.local_path == t.local
        && w.account_id == t.acct
        && w.folder_id == t.folder
        && w.dest_label == t.label
        && w.direction == DIR_MIRROR
        && w.on_change == t.rule
}

/// Turns Portal's rows into the watches they call for, reporting (not acting on)
/// the ones that cannot be.
fn plan(links: &[UploadLink], root: &Path, rule: &str, rep: &mut Report) -> Vec<Target> {
    let mut out: Vec<Target> = Vec::new();
    let mut claimed: BTreeSet<String> = BTreeSet::new();
    // Sorted by id so two runs over the same rows decide the same way.
    let mut by_id: BTreeMap<String, &UploadLink> = BTreeMap::new();
    for l in links {
        let id = l.id.trim().to_string();
        if id.is_empty() {
            rep.skipped.push(Skip {
                link: String::new(),
                marker: l.marker(),
                reason: "the link has no id, so there is nothing to own a watch by".to_string(),
            });
            continue;
        }
        by_id.entry(id).or_insert(l);
    }

    for (id, l) in by_id {
        let marker = l.marker();
        let skip = |reason: String| Skip { link: id.clone(), marker: marker.clone(), reason };
        if marker.is_empty() {
            // Portal is not asking for a watch: no target, and any watch this
            // link used to own is removed by `apply`.
            continue;
        }
        if l.is_disabled() {
            rep.skipped.push(skip("the link is not live".to_string()));
            continue;
        }
        if let Err(e) = check_marker(&marker) {
            rep.skipped.push(skip(e));
            continue;
        }
        let (acct, folder) = (l.acct(), l.dest_folder());
        if acct.is_empty() || folder.is_empty() {
            rep.skipped.push(skip(
                "the link names no Frame.io account (or folder): Portal's /api/upload-links row seen here has no account_id"
                    .to_string(),
            ));
            continue;
        }
        let local: PathBuf = root.join(&marker);
        // Belt and braces: the join must stay directly under the root. A marker
        // that slipped past `check_marker` would otherwise be able to leave it.
        if local.parent() != Some(root) {
            rep.skipped.push(skip(format!("{} is not directly under {}", local.display(), root.display())));
            continue;
        }
        let local = local.to_string_lossy().into_owned();
        if !claimed.insert(local.clone()) {
            rep.skipped.push(skip(format!("{} is another link's folder already", local)));
            continue;
        }
        out.push(Target {
            link: id,
            marker,
            local,
            acct,
            folder,
            label: l.label(),
            rule: rule.to_string(),
        });
    }
    out
}

/// Starts the poller: every `Settings::portal_every_secs`, while Portal is on.
///
/// It reads the settings each time round, so a changed interval or a portal that
/// was switched off takes effect within one interval. One reconcile at a time is
/// `App::portal_reconcile`'s job, not this loop's.
pub fn spawn(app: Arc<App>) {
    tokio::spawn(async move {
        loop {
            tokio::time::sleep(app.settings().portal_interval()).await;
            let s = app.settings();
            if !s.portal_enabled || s.portal_url.trim().is_empty() {
                continue;
            }
            let _ = app.portal_reconcile().await;
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::history::watch::{ON_CHANGE_KEEP, ON_CHANGE_OVERWRITE};

    #[test]
    fn a_marker_is_exactly_one_plain_segment() {
        for good in ["Rakete", "projekt-42", "Ümlaut", ".hidden", "a b", "x.y"] {
            assert!(check_marker(good).is_ok(), "{:?} is a folder name", good);
        }
        for bad in ["", ".", "..", "/", "a/b", "..%2f..", "a\\b", "a%b", "a\nb", "a\u{0}b", "a\tb"] {
            assert!(check_marker(bad).is_err(), "{:?} must be refused", bad);
        }
        assert!(check_marker(&"x".repeat(MAX_MARKER + 1)).is_err());
        assert!(check_marker(&"x".repeat(MAX_MARKER)).is_ok());
    }

    #[test]
    fn origin_round_trips() {
        assert_eq!(origin_of("L1"), "portal:L1");
        assert_eq!(link_of("portal:L1"), Some("L1"));
        assert_eq!(link_of(""), None);
        assert_eq!(link_of("user"), None);
        assert_eq!(link_of("Portal:L1"), None, "the prefix is exact");
    }

    fn link(id: &str, marker: &str) -> UploadLink {
        UploadLink {
            id: id.to_string(),
            name: format!("Link {}", id),
            target_folder_id: format!("F{}", id),
            target_account_id: "A1".to_string(),
            turbosort: marker.to_string(),
            ..Default::default()
        }
    }

    fn plan_of(links: &[UploadLink]) -> (Vec<Target>, Report) {
        let mut rep = Report::default();
        let root = Path::new("/root/Live_Work");
        let t = plan(links, root, ON_CHANGE_OVERWRITE, &mut rep);
        (t, rep)
    }

    #[test]
    fn a_valid_link_plans_one_mirrored_watch_under_the_root() {
        let (t, rep) = plan_of(&[link("L1", "Rakete")]);
        assert_eq!(t.len(), 1);
        assert!(rep.skipped.is_empty());
        assert_eq!(t[0].local, "/root/Live_Work/Rakete");
        assert_eq!(t[0].acct, "A1");
        assert_eq!(t[0].folder, "FL1");
        assert_eq!(t[0].label, "Link L1");
        assert_eq!(t[0].rule, ON_CHANGE_OVERWRITE);
    }

    #[test]
    fn an_empty_or_broken_marker_plans_nothing_and_says_why() {
        let (t, rep) = plan_of(&[link("L1", ""), link("L2", ".."), link("L3", "a/b"), link("L4", "ok")]);
        assert_eq!(t.len(), 1, "only the good marker plans a watch");
        assert_eq!(t[0].link, "L4");
        assert_eq!(rep.skipped.len(), 2, "an empty marker is not a skip: Portal simply asked for nothing");
        assert_eq!(rep.skipped.iter().map(|s| s.link.as_str()).collect::<Vec<_>>(), vec!["L2", "L3"]);
    }

    #[test]
    fn two_links_naming_one_folder_plan_one_watch() {
        let (t, rep) = plan_of(&[link("L1", "Same"), link("L2", "Same")]);
        assert_eq!(t.len(), 1);
        assert_eq!(t[0].link, "L1", "the lower id wins, run after run");
        assert_eq!(rep.skipped.len(), 1);
        assert!(rep.skipped[0].reason.contains("another link's folder"));
    }

    #[test]
    fn a_link_without_an_account_plans_nothing() {
        let mut l = link("L1", "X");
        l.target_account_id.clear();
        let (t, rep) = plan_of(&[l]);
        assert!(t.is_empty());
        assert!(rep.skipped[0].reason.contains("no Frame.io account"));
    }

    #[test]
    fn a_link_that_is_off_plans_nothing() {
        let mut l = link("L1", "X");
        l.status = "archived".to_string();
        let (t, _) = plan_of(&[l]);
        assert!(t.is_empty());
    }

    #[test]
    fn the_change_rule_is_the_one_asked_for() {
        let mut rep = Report::default();
        let t = plan(&[link("L1", "X")], Path::new("/r"), ON_CHANGE_KEEP, &mut rep);
        assert_eq!(t[0].rule, ON_CHANGE_KEEP);
        assert_eq!(ON_CHANGE_KEEP, "keep");
    }

    #[test]
    fn a_watch_matches_only_when_every_field_is_right() {
        let t = Target {
            link: "L1".into(),
            marker: "X".into(),
            local: "/r/X".into(),
            acct: "A".into(),
            folder: "F".into(),
            label: "N".into(),
            rule: ON_CHANGE_OVERWRITE.into(),
        };
        let mut w = Watch {
            local_path: "/r/X".into(),
            account_id: "A".into(),
            folder_id: "F".into(),
            dest_label: "N".into(),
            direction: DIR_MIRROR.into(),
            on_change: ON_CHANGE_OVERWRITE.into(),
            ..Default::default()
        };
        assert!(matches(&w, &t));
        w.enabled = false;
        assert!(matches(&w, &t), "a paused watch is not edited back on");
        w.direction = crate::history::watch::DIR_UPLOAD.into();
        assert!(!matches(&w, &t));
        w.direction = DIR_MIRROR.into();
        w.on_change = ON_CHANGE_KEEP.into();
        assert!(!matches(&w, &t));
    }
}
