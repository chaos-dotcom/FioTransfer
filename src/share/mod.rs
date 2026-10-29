//! Download sources: Frame.io shares and review links, Smash, WeTransfer,
//! Dropbox, Google Drive and Portal links.

pub mod dropbox;
pub mod gdrive;
pub mod legacy;
pub mod portal;
pub mod smash;
pub mod wetransfer;

use base64::Engine as _;
use futures::future::BoxFuture;
use once_cell::sync::Lazy;
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use std::future::Future;
use std::sync::{Arc, Mutex};
use tokio_util::sync::CancellationToken;

use crate::netx;

/// Where the GraphQL endpoint lives (overridable for tests).
pub fn graphql_url() -> String {
    std::env::var("FIO_GRAPHQL_URL").unwrap_or_else(|_| "https://api.frame.io/graphql".to_string())
}

pub const ERR_PASSPHRASE: &str = "passphrase required";
pub const ERR_BAD_PASSPHRASE: &str = "wrong passphrase";
pub const ERR_DOWNLOAD_DISABLED: &str = "downloads disabled by sender";
pub const ERR_INVALID_SHARE: &str = "link not found or expired";
pub const ERR_NOT_A_SHARE_LINK: &str =
    "not a Frame.io, Smash, WeTransfer, Dropbox, Google Drive or Portal link";

pub const KIND_SHARE: &str = "share"; // Frame.io V4 share (next.frame.io/share/<id>)
pub const KIND_REVIEW: &str = "review"; // legacy Frame.io review link (app.frame.io/reviews/<id>)
pub const KIND_SMASH: &str = "smash";
pub const KIND_WETRANSFER: &str = "wetransfer";
pub const KIND_DROPBOX: &str = "dropbox";
pub const KIND_DRIVE: &str = "drive";
pub const KIND_PORTAL: &str = "portal";

#[derive(Debug, Clone, Serialize, Deserialize, Default, PartialEq)]
pub struct Link {
    pub kind: String,
    pub id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub url: String,
}

impl Link {
    /// Label names a share that has no name of its own (its download folder).
    pub fn label(&self) -> String {
        let short: String = self.id.chars().take(8).collect();
        match self.kind.as_str() {
            KIND_SMASH => return format!("Smash {}", self.id),
            KIND_WETRANSFER => return format!("WeTransfer {}", short),
            KIND_DROPBOX => return format!("Dropbox {}", short),
            KIND_DRIVE => return format!("Google Drive {}", short),
            KIND_PORTAL => return format!("Portal {}", short),
            _ => {}
        }
        format!("Frame.io share {}", short)
    }
}

/// Who is downloading, for portal links that ask.
pub type Viewer = Arc<dyn Fn() -> Option<(String, String)> + Send + Sync>;

pub static VIEWER: Mutex<Option<Viewer>> = Mutex::new(None);

#[derive(Debug, Clone, Default)]
pub struct Info {
    pub share_id: String,
    pub name: String,
    pub downloading_enabled: bool,
}

#[derive(Debug, Clone, Default)]
pub struct File {
    /// The asset to re-query for a fresh URL (version stack head or file).
    pub asset_id: String,
    /// Path relative to the share root, using "/".
    pub rel_path: String,
    pub size: i64,
    pub url: String,
    /// Non-empty if this file can't be downloaded (e.g. watermarked).
    pub problem: String,
}

/// Something that can list a share's files and renew their download URLs.
#[async_trait::async_trait]
pub trait Source: Send + Sync {
    async fn info(&self, cx: &CancellationToken) -> Result<Info, String>;
    async fn list(
        &self,
        cx: &CancellationToken,
        progress: &(dyn Fn(usize) + Send + Sync),
    ) -> Result<Vec<File>, String>;
    async fn fresh_url(&self, cx: &CancellationToken, asset: &str) -> Result<String, String>;
    async fn mark_downloaded(&self, cx: &CancellationToken, ids: &[String]) -> Result<(), String>;
}

/// Open returns the right client for a resolved link.
pub fn open(l: &Link, passphrase: &str) -> Box<dyn Source> {
    match l.kind.as_str() {
        KIND_REVIEW => Box::new(legacy::LegacyClient { review_id: l.id.clone(), passphrase: passphrase.to_string() }),
        KIND_SMASH => Box::new(smash::SmashClient { transfer_id: l.id.clone(), passphrase: passphrase.to_string() }),
        KIND_WETRANSFER => Box::new(wetransfer::new_we_transfer(l, passphrase)),
        KIND_DROPBOX => Box::new(dropbox::DropboxClient::new(l.url.clone())),
        KIND_DRIVE => Box::new(gdrive::new_drive(l)),
        KIND_PORTAL => Box::new(portal::PortalClient::new(l.url.clone(), passphrase)),
        _ => Box::new(Client { share_id: l.id.clone(), passphrase: passphrase.to_string() }),
    }
}

/// ResolveLink turns any Frame.io link (next.frame.io/share/<id>,
/// app.frame.io/reviews/<id>, f.io/<code>, or a bare V4 share ID), Smash,
/// WeTransfer, Dropbox, Google Drive or Portal link into a Link, following
/// redirects for short links.
pub async fn resolve_link(cx: &CancellationToken, raw: &str) -> Result<Link, String> {
    let raw = raw.trim();
    if let Some(l) = link_from_url(raw) {
        return Ok(l);
    }
    if let Some(t) = smash::smash_target(raw) {
        return smash::resolve_smash(cx, &t).await;
    }
    if raw.len() == 36 && is_uuid(raw) {
        return Ok(Link { kind: KIND_SHARE.to_string(), id: raw.to_lowercase(), url: String::new() });
    }
    let with_scheme = if raw.contains("://") { raw.to_string() } else { format!("https://{}", raw) };
    let u = match url::Url::parse(&with_scheme) {
        Ok(u) => u,
        Err(_) => return Err(ERR_NOT_A_SHARE_LINK.to_string()),
    };
    if !u.host_str().map(|h| h.contains('.')).unwrap_or(false) {
        return Err(ERR_NOT_A_SHARE_LINK.to_string());
    }
    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(30))
        .redirect(reqwest::redirect::Policy::custom(|attempt| {
            let s = attempt.url().as_str();
            if link_from_url(s).is_some() || smash::smash_target(s).is_some() || attempt.previous().len() > 10 {
                attempt.stop()
            } else {
                attempt.follow()
            }
        }))
        .build()
        .map_err(|e| e.to_string())?;
    let resp = client
        .get(u)
        .header("User-Agent", "Mozilla/5.0 (Macintosh) FioTransfer")
        .send()
        .await
        .map_err(|e| format!("opening link: {}", e))?;
    let final_url = resp.url().clone();
    let mut hop: Vec<String> = Vec::new();
    if let Some(l) = resp.headers().get(reqwest::header::LOCATION).and_then(|v| v.to_str().ok()) {
        hop.push(l.to_string());
    }
    hop.push(final_url.as_str().to_string());
    for s in hop {
        if let Some(l) = link_from_url(&s) {
            return Ok(l);
        }
        if let Some(t) = smash::smash_target(&s) {
            return smash::resolve_smash(cx, &t).await;
        }
    }
    // Last resort: look for a share URL inside the page.
    let body = read_limited(resp, 2 << 20).await;
    if let Some(l) = in_page_link(&body) {
        return Ok(l);
    }
    if let Some(l) = portal::portal_link(cx, &final_url).await {
        return Ok(l);
    }
    // A Smash transfer on the sender's own domain.
    let path = final_url.path().to_string();
    let trimmed = path.strip_suffix('/').unwrap_or(&path).to_string();
    if body.windows(smash::SMASH_PAGE_MARKER.len()).any(|w| w == smash::SMASH_PAGE_MARKER) && !trimmed.is_empty() {
        let host = final_url.host_str().unwrap_or("").to_lowercase();
        return smash::resolve_smash(cx, &format!("{}{}", host, trimmed)).await;
    }
    Err(ERR_NOT_A_SHARE_LINK.to_string())
}

/// LooksLikeLink tells, without opening it, whether raw is a link of a known
/// kind: a service's own URL, one of their short links, or a Portal-shaped
/// /d/<token>. Used to pick up a copied link: opening any copied URL could
/// spend a one-time link.
pub fn looks_like_link(raw: &str) -> bool {
    let raw = raw.trim();
    if raw.is_empty() || raw.contains([' ', '\t', '\r', '\n']) {
        return false;
    }
    if link_from_url(raw).is_some() {
        return true;
    }
    if let Some(t) = smash::smash_target(raw) {
        return !t.is_empty();
    }
    let u = match parse_link(raw) {
        Some(u) => u,
        None => return false,
    };
    let p = path_parts(&u);
    match u.host_str().unwrap_or("") {
        "f.io" | "we.tl" => return !p.is_empty(),
        _ => {}
    }
    p.len() == 2 && p[0] == "d" && portal::portal_token_ok(&p[1])
}

/// linkFromURL recognises a link from its URL alone (Smash links need a
/// lookup: smash_target).
pub(crate) fn link_from_url(s: &str) -> Option<Link> {
    for (marker, kind) in [("/share/", KIND_SHARE), ("/reviews/", KIND_REVIEW)] {
        if let Some(i) = s.find(marker) {
            let rest = &s[i + marker.len()..];
            if let Some(m) = uuid_at(rest, 0) {
                return Some(Link { kind: kind.to_string(), id: m.to_lowercase(), url: String::new() });
            }
        }
    }
    if let Some(l) = wetransfer::we_transfer_link(s) {
        return Some(l);
    }
    if let Some(l) = dropbox::dropbox_link(s) {
        return Some(l);
    }
    if let Some(l) = gdrive::drive_link(s) {
        return Some(l);
    }
    None
}

/// The first share-shaped URL inside a page: "/share/<uuid>" or "/reviews/<uuid>".
fn in_page_link(body: &[u8]) -> Option<Link> {
    let text = String::from_utf8_lossy(body);
    let mut best: Option<(usize, &str, &str)> = None;
    for (marker, kind) in [("/share/", KIND_SHARE), ("/reviews/", KIND_REVIEW)] {
        let mut from = 0;
        while let Some(i) = text[from..].find(marker) {
            let at = from + i;
            let rest = &text[at + marker.len()..];
            if let Some(m) = uuid_at(rest, 0) {
                if best.map(|(b, _, _)| at < b).unwrap_or(true) {
                    best = Some((at, kind, m));
                }
                break;
            }
            from = at + marker.len();
        }
    }
    best.map(|(_, kind, id)| Link { kind: kind.to_string(), id: id.to_lowercase(), url: String::new() })
}

/// True when s starts with a 8-4-4-4-12 hex UUID; returns it.
pub(crate) fn uuid_at(s: &str, i: usize) -> Option<&str> {
    let b = s.as_bytes();
    if i.checked_add(36).map(|e| e > b.len()).unwrap_or(true) {
        return None;
    }
    let mut pos = i;
    for (k, len) in [8usize, 4, 4, 4, 12].into_iter().enumerate() {
        if k > 0 {
            if b[pos] != b'-' {
                return None;
            }
            pos += 1;
        }
        for _ in 0..len {
            if !b[pos].is_ascii_hexdigit() {
                return None;
            }
            pos += 1;
        }
    }
    Some(&s[i..i + 36])
}

fn is_uuid(s: &str) -> bool {
    s.len() == 36 && uuid_at(s, 0).is_some()
}

/// parseLink parses a pasted link, which may lack its scheme, with the host
/// lowercased.
pub(crate) fn parse_link(s: &str) -> Option<url::Url> {
    let full = if s.contains("://") { s.to_string() } else { format!("https://{}", s) };
    let mut u = url::Url::parse(&full).ok()?;
    let host = u.host_str()?.to_lowercase();
    u.set_host(Some(&host)).ok()?;
    Some(u)
}

/// pathParts splits a URL path into its non-empty parts.
pub(crate) fn path_parts(u: &url::Url) -> Vec<String> {
    match u.path_segments() {
        Some(segs) => segs.filter(|p| !p.is_empty()).map(|p| p.to_string()).collect(),
        None => Vec::new(),
    }
}

/// origin is Go's u.Scheme + "://" + u.Host: the port is part of it when there is one.
pub(crate) fn origin(u: &url::Url) -> String {
    let host = u.host_str().unwrap_or("");
    match u.port() {
        Some(p) => format!("{}://{}:{}", u.scheme(), host, p),
        None => format!("{}://{}", u.scheme(), host),
    }
}

/// pathJoin is Go's path.Join for the two-part names this package builds.
pub(crate) fn path_join(prefix: &str, name: &str) -> String {
    if prefix.is_empty() {
        name.to_string()
    } else if name.is_empty() {
        prefix.to_string()
    } else {
        format!("{}/{}", prefix.trim_end_matches('/'), name)
    }
}

/// webUA is a browser user agent, for services that turn other clients away.
pub(crate) const WEB_UA: &str =
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36";

/// queryEscape is Go's url.QueryEscape: everything but unreserved characters
/// is escaped, and a space becomes "+".
pub(crate) fn query_escape(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for &b in s.as_bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => out.push(b as char),
            b' ' => out.push('+'),
            _ => out.push_str(&format!("%{:02X}", b)),
        }
    }
    out
}

/// pathEscape is Go's url.PathEscape, for one path segment.
pub(crate) fn path_escape(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for &b in s.as_bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => out.push(b as char),
            b'$' | b'&' | b'+' | b':' | b'=' | b'@' => out.push(b as char),
            _ => out.push_str(&format!("%{:02X}", b)),
        }
    }
    out
}

/// encodeQuery is Go's url.Values.Encode: keys sorted, both sides QueryEscaped.
pub(crate) fn encode_query(pairs: &[(String, String)]) -> String {
    let mut sorted: Vec<&(String, String)> = pairs.iter().collect();
    sorted.sort_by(|a, b| a.0.cmp(&b.0));
    let mut out = String::new();
    for (k, v) in sorted {
        if !out.is_empty() {
            out.push('&');
        }
        out.push_str(&query_escape(k));
        out.push('=');
        out.push_str(&query_escape(v));
    }
    out
}

/// Reads at most `limit` bytes of a response body, as io.LimitReader does:
/// a read error keeps what came before it.
pub(crate) async fn read_limited(mut resp: reqwest::Response, limit: usize) -> Vec<u8> {
    let mut out: Vec<u8> = Vec::new();
    while out.len() < limit {
        match resp.chunk().await {
            Ok(Some(chunk)) => {
                let take = (limit - out.len()).min(chunk.len());
                out.extend_from_slice(&chunk[..take]);
            }
            Ok(None) => break,
            Err(_) => break,
        }
    }
    out
}

/// send performs the request newReq builds, retrying network errors, 429 and
/// 5xx with backoff, and returns the last response's status and body (the
/// headers none of these services look at).
pub(crate) async fn send<F>(
    cx: &CancellationToken,
    client: &reqwest::Client,
    service: &str,
    mut new_req: F,
) -> Result<(u16, Vec<u8>), String>
where
    F: FnMut() -> reqwest::RequestBuilder,
{
    let mut last_err = String::new();
    for attempt in 0..6 {
        let req = match new_req().build() {
            Ok(r) => r,
            Err(e) => return Err(e.to_string()),
        };
        match client.execute(req).await {
            Ok(resp) => {
                let status = resp.status().as_u16();
                let data = read_limited(resp, 64 << 20).await;
                if status != 429 && status < 500 {
                    return Ok((status, data));
                }
                last_err = format!("{} returned {}", service, status);
            }
            Err(e) => last_err = e.to_string(),
        }
        netx_sleep(cx, netx::backoff(attempt + 1)).await?;
    }
    Err(last_err)
}

/// netx_sleep waits, and reports Go's context.Canceled message when cancelled.
pub(crate) async fn netx_sleep(cx: &CancellationToken, d: std::time::Duration) -> Result<(), String> {
    netx::sleep(cx, d).await.map_err(|e| e.to_string())
}

/// listConcurrency bounds the requests one listing has in flight.
pub(crate) const LIST_CONCURRENCY: usize = 6;

/// lister walks a share's folder tree with several requests in flight. Callers
/// keep results in listing order: dedupe_paths renames by position, so the same
/// share must always list the same way.
pub(crate) struct Lister<'p> {
    cx: CancellationToken,
    sem: tokio::sync::Semaphore,
    n: Mutex<usize>,
    progress: &'p (dyn Fn(usize) + Send + Sync),
    cause: Mutex<Option<String>>,
}

impl<'p> Lister<'p> {
    pub fn new(cx: &CancellationToken, progress: &'p (dyn Fn(usize) + Send + Sync)) -> Self {
        Lister {
            cx: cx.child_token(),
            sem: tokio::sync::Semaphore::new(LIST_CONCURRENCY),
            n: Mutex::new(0),
            progress,
            cause: Mutex::new(None),
        }
    }

    /// token is the lister's own cancellation token: cancelled when the caller
    /// is, or when the listing fails.
    pub fn token(&self) -> &CancellationToken {
        &self.cx
    }

    /// cause is the first failure seen, which is the error a listing reports.
    pub fn cause(&self) -> Option<String> {
        self.cause.lock().ok().and_then(|g| g.clone())
    }

    /// fail records a failure once and stops the rest.
    pub fn fail(&self, e: &str) {
        if let Ok(mut g) = self.cause.lock() {
            if g.is_none() {
                *g = Some(e.to_string());
                self.cx.cancel();
            }
        }
    }

    /// call runs one request within the concurrency bound.
    pub async fn call<F, T>(&self, f: F) -> Result<T, String>
    where
        F: Future<Output = Result<T, String>>,
    {
        let permit = tokio::select! {
            p = self.sem.acquire() => match p {
                Ok(p) => p,
                Err(_) => return Err(self.cause().unwrap_or_else(|| "context canceled".to_string())),
            },
            _ = self.cx.cancelled() => {
                return Err(self.cause().unwrap_or_else(|| "context canceled".to_string()))
            }
        };
        let out = f.await;
        drop(permit);
        if let Err(e) = &out {
            self.fail(e);
        }
        out
    }

    /// found reports n more files to the progress callback.
    pub fn found(&self, n: usize) {
        if n == 0 {
            return;
        }
        let total = match self.n.lock() {
            Ok(mut g) => {
                *g += n;
                *g
            }
            Err(_) => return,
        };
        (self.progress)(total);
    }
}

impl Drop for Lister<'_> {
    fn drop(&mut self) {
        self.cx.cancel();
    }
}

// ---------- Time ----------

/// parse_rfc3339 reads an RFC 3339 timestamp into Unix seconds, as
/// time.Parse(time.RFC3339) does: "2026-10-23T12:00:00Z" or a "+02:00" offset.
pub(crate) fn parse_rfc3339(s: &str) -> Option<i64> {
    fn digits(b: &[u8], at: usize, n: usize) -> Option<i64> {
        let mut v: i64 = 0;
        for k in 0..n {
            let c = *b.get(at + k)?;
            if !c.is_ascii_digit() {
                return None;
            }
            v = v * 10 + (c - b'0') as i64;
        }
        Some(v)
    }
    let b = s.as_bytes();
    if b.len() < 19 {
        return None;
    }
    let year = digits(b, 0, 4)?;
    let month = digits(b, 5, 2)?;
    let day = digits(b, 8, 2)?;
    if b[4] != b'-' || b[7] != b'-' {
        return None;
    }
    if b[10] != b'T' && b[10] != b't' && b[10] != b' ' {
        return None;
    }
    let hour = digits(b, 11, 2)?;
    let min = digits(b, 14, 2)?;
    let sec = digits(b, 17, 2)?;
    if b[13] != b':' || b[16] != b':' {
        return None;
    }
    let mut i = 19;
    if i < b.len() && b[i] == b'.' {
        i += 1;
        while i < b.len() && b[i].is_ascii_digit() {
            i += 1;
        }
    }
    let mut offset: i64 = 0;
    if i < b.len() {
        match b[i] {
            b'Z' | b'z' => i += 1,
            sign @ (b'+' | b'-') => {
                let oh = digits(b, i + 1, 2)?;
                if *b.get(i + 3)? != b':' {
                    return None;
                }
                let om = digits(b, i + 4, 2)?;
                offset = if sign == b'-' { -(oh * 3600 + om * 60) } else { oh * 3600 + om * 60 };
                i += 6;
            }
            _ => return None,
        }
    }
    if i != b.len() {
        return None;
    }
    // Howard Hinnant's days-from-civil.
    let y = if month <= 2 { year - 1 } else { year };
    let era = if y >= 0 { y } else { y - 399 } / 400;
    let yoe = y - era * 400;
    let mp = (month + 9) % 12;
    let doy = (153 * mp + 2) / 5 + day - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    let days = era * 146097 + doe - 719468;
    Some(days * 86400 + hour * 3600 + min * 60 + sec - offset)
}

/// now_unix is the clock, in Unix seconds.
pub(crate) fn now_unix() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

// ---------- Names ----------

/// SafeName turns a name from Frame.io into a single safe path component.
///
/// Names safe on this filesystem.
pub fn safe_name(s: &str) -> String {
    let s = s.replace('/', "_").replace(':', "_").replace('\0', "");
    let s = s.trim().to_string();
    if s.is_empty() || s == "." || s == ".." {
        return "untitled".to_string();
    }
    s
}

/// relPath turns a file name that may carry folders ("Day 1/a.mov") into a safe
/// relative path.
pub(crate) fn rel_path(name: &str) -> String {
    let mut parts: Vec<String> = Vec::new();
    for p in name.split('/') {
        let p = p.trim();
        if p.is_empty() || p == "." || p == ".." {
            continue;
        }
        parts.push(safe_name(p));
    }
    if parts.is_empty() {
        return "untitled".to_string();
    }
    parts.join("/")
}

/// pathExt is Go's path.Ext: the suffix from the last dot of the final element.
fn path_ext(p: &str) -> &str {
    let start = p.rfind('/').map(|i| i + 1).unwrap_or(0);
    match p[start..].rfind('.') {
        Some(j) => &p[start + j..],
        None => "",
    }
}

/// dedupePaths renames files whose relative paths collide ("a.mov", "a (2).mov"),
/// ignoring case as macOS does. A generated name never takes a path another
/// file in the share already has.
pub(crate) fn dedupe_paths(files: &mut [File]) {
    use std::collections::HashSet;
    let mut taken: HashSet<String> = HashSet::with_capacity(files.len());
    for f in files.iter() {
        taken.insert(f.rel_path.to_lowercase());
    }
    let mut used: HashSet<String> = HashSet::with_capacity(files.len());
    for i in 0..files.len() {
        let p = files[i].rel_path.to_lowercase();
        if used.insert(p) {
            continue;
        }
        let ext = path_ext(&files[i].rel_path).to_string();
        let base = if ext.is_empty() {
            files[i].rel_path.clone()
        } else {
            files[i].rel_path[..files[i].rel_path.len() - ext.len()].to_string()
        };
        let mut n = 2usize;
        loop {
            let c = format!("{} ({}){}", base, n, ext);
            let lc = c.to_lowercase();
            if !taken.contains(&lc) {
                files[i].rel_path = c;
                taken.insert(lc.clone());
                used.insert(lc);
                break;
            }
            n += 1;
        }
    }
}

// ---------- Frame.io V4 share client (GraphQL) ----------

#[derive(Debug, Clone, Default, Deserialize)]
struct GqlExtensions {
    #[serde(default)]
    code: String,
}

#[derive(Debug, Clone, Default, Deserialize)]
struct GqlError {
    #[serde(default)]
    message: String,
    #[serde(default)]
    extensions: GqlExtensions,
}

#[derive(Debug, Default, Deserialize)]
struct Envelope {
    #[serde(default)]
    data: Option<serde_json::Value>,
    #[serde(default)]
    errors: Vec<GqlError>,
}

/// Client talks to the Frame.io GraphQL endpoint with share credentials.
#[derive(Debug, Clone, Default)]
pub struct Client {
    pub share_id: String,
    pub passphrase: String,
}

impl Client {
    async fn query<T: DeserializeOwned>(
        &self,
        cx: &CancellationToken,
        op: &str,
        q: &str,
        vars: serde_json::Value,
    ) -> Result<T, String> {
        let body = serde_json::to_vec(&serde_json::json!({
            "operationName": op,
            "query": q,
            "variables": vars,
        }))
        .unwrap_or_default();
        let cred = if self.passphrase.is_empty() {
            self.share_id.clone()
        } else {
            format!("{}:{}", self.share_id, self.passphrase)
        };
        let auth = BASE64.encode(cred.as_bytes());
        let mut last_err = String::new();
        for attempt in 0..6 {
            let req = netx::api_client()
                .post(graphql_url())
                .header("Content-Type", "application/json")
                .header("apollographql-client-name", "web-app")
                .header("x-gql-op", op)
                .header("x-frameio-share-authentication", &auth)
                .body(body.clone());
            let resp = match req.send().await {
                Ok(r) => r,
                Err(e) => {
                    last_err = e.to_string();
                    netx_sleep(cx, netx::backoff(attempt + 1)).await?;
                    continue;
                }
            };
            let status = resp.status().as_u16();
            let data = read_limited(resp, 64 << 20).await;
            if status == 429 || status >= 500 {
                last_err = format!("Frame.io returned {}", status);
                netx_sleep(cx, netx::backoff(attempt + 1)).await?;
                continue;
            }
            let env: Envelope = match serde_json::from_slice(&data) {
                Ok(e) => e,
                Err(_) => return Err(format!("unexpected response from Frame.io ({})", status)),
            };
            if let Some(e) = env.errors.first() {
                return Err(map_error(e));
            }
            let d = env.data.unwrap_or(serde_json::Value::Null);
            if d.is_null() {
                // Go unmarshals a missing "data" and gets the JSON reader's error.
                return Err("unexpected end of JSON input".to_string());
            }
            return serde_json::from_value(d).map_err(|e| e.to_string());
        }
        Err(last_err)
    }
}

/// mapError turns a GraphQL error into the message the UI shows.
fn map_error(e: &GqlError) -> String {
    let m = e.message.to_lowercase();
    if m.contains("invalid share passphrase") || m.contains("incorrect passphrase") {
        return ERR_BAD_PASSPHRASE.to_string();
    }
    if m.contains("passphrase") {
        return ERR_PASSPHRASE.to_string();
    }
    if m.contains("invalid share") || e.extensions.code == "NOT_FOUND" {
        return ERR_INVALID_SHARE.to_string();
    }
    format!("Frame.io: {} ({})", e.message, e.extensions.code)
}

const DL_FRAGMENT: &str = r#"
fragment FioDL on Asset {
  ... on VideoAsset { media { original { downloadUrl filesizeInBytes } } }
  ... on AudioAsset { media { original { downloadUrl filesizeInBytes } } }
  ... on ImageAsset { media { original { downloadUrl filesizeInBytes } } }
  ... on DocumentAsset { media { original { downloadUrl filesizeInBytes } } }
  ... on ModelAsset { media { original { downloadUrl filesizeInBytes } } }
  ... on UnsupportedAsset { media { original { downloadUrl filesizeInBytes } } }
  ... on InteractiveAsset { media { original { downloadUrl filesizeInBytes } } }
}"#;

const INFO_QUERY: &str = r#"query FioShare($shareId: ID!) {
  share(shareId: $shareId) { __typename id ... on Share { downloadingEnabled enabled expiration name } }
}"#;

const LIST_QUERY_BASE: &str = r#"query FioShareAssets($shareId: ID!, $folderId: ID, $page: PageInput!) {
  share(shareId: $shareId) { id ... on AssetAggregate { assets(folderId: $folderId, page: $page) {
    nodes { __typename id name ...FioDL
      ... on VersionStackAsset { versions { __typename id name versionNumber ...FioDL } } }
    pageInfo { endCursor hasNextPage } } } }
}"#;

const BY_IDS_QUERY_BASE: &str = r#"query FioShareAssetsByIds($shareId: ID!, $assetIds: [ID!]!, $first: NonNegativeInt!) {
  share(by: {shareId: $shareId}) { id assets(filter: {assetIds: $assetIds}, page: {first: $first}) {
    nodes { __typename id name ...FioDL
      ... on VersionStackAsset { versions { __typename id name versionNumber ...FioDL } } } } }
}"#;

const MARK_MUTATION: &str = r#"mutation MarkSharedAssetsDownloaded($input: MarkSharedAssetsDownloadedInput!) {
  markSharedAssetsDownloaded(input: $input) { successful }
}"#;

static LIST_QUERY: Lazy<String> = Lazy::new(|| format!("{}{}", LIST_QUERY_BASE, DL_FRAGMENT));
static BY_IDS_QUERY: Lazy<String> = Lazy::new(|| format!("{}{}", BY_IDS_QUERY_BASE, DL_FRAGMENT));

#[derive(Debug, Clone, Default, Deserialize)]
struct Media {
    #[serde(default)]
    original: Option<Original>,
}

#[derive(Debug, Clone, Default, Deserialize)]
struct Original {
    #[serde(rename = "downloadUrl", default)]
    download_url: Option<String>,
    #[serde(rename = "filesizeInBytes", default)]
    filesize_in_bytes: Option<i64>,
}

#[derive(Debug, Clone, Default, Deserialize)]
struct Node {
    #[serde(rename = "__typename", default)]
    typename: String,
    #[serde(default)]
    id: String,
    #[serde(default)]
    name: String,
    #[serde(rename = "versionNumber", default)]
    version_number: Option<i64>,
    #[serde(default)]
    media: Option<Media>,
    #[serde(default)]
    versions: Option<Vec<Node>>,
}

impl Node {
    /// head returns the asset to download: the node itself, or a version
    /// stack's newest version.
    fn head(&self) -> &Node {
        let versions = match &self.versions {
            Some(v) if self.typename == "VersionStackAsset" && !v.is_empty() => v,
            _ => return self,
        };
        let mut best = &versions[versions.len() - 1];
        for v in versions {
            if let Some(n) = v.version_number {
                if best.version_number.map(|b| n > b).unwrap_or(true) {
                    best = v;
                }
            }
        }
        best
    }

    fn url(&self) -> (String, i64) {
        let original = match self.media.as_ref().and_then(|m| m.original.as_ref()) {
            Some(o) => o,
            None => return (String::new(), 0),
        };
        let url = match &original.download_url {
            Some(u) => u.clone(),
            None => return (String::new(), 0),
        };
        (url, original.filesize_in_bytes.unwrap_or(0))
    }
}

#[derive(Debug, Default, Deserialize)]
struct InfoReply {
    #[serde(default)]
    share: Option<ShareInfo>,
}

#[derive(Debug, Default, Deserialize)]
struct ShareInfo {
    #[serde(default)]
    id: String,
    #[serde(default)]
    name: Option<String>,
    #[serde(rename = "downloadingEnabled", default)]
    downloading_enabled: Option<bool>,
    #[serde(default)]
    enabled: Option<bool>,
}

#[derive(Debug, Default, Deserialize)]
struct PageInfo {
    #[serde(rename = "endCursor", default)]
    end_cursor: Option<String>,
    #[serde(rename = "hasNextPage", default)]
    has_next_page: bool,
}

#[derive(Debug, Default, Deserialize)]
struct AssetsReply {
    #[serde(default)]
    nodes: Vec<Node>,
    #[serde(rename = "pageInfo", default)]
    page_info: PageInfo,
}

#[derive(Debug, Default, Deserialize)]
struct AssetsShare {
    #[serde(default)]
    assets: Option<AssetsReply>,
}

#[derive(Debug, Default, Deserialize)]
struct ListReply {
    #[serde(default)]
    share: Option<AssetsShare>,
}

#[derive(Debug, Default, Deserialize)]
struct ByIdsShare {
    #[serde(default)]
    assets: Option<IdsNodes>,
}

#[derive(Debug, Default, Deserialize)]
struct IdsNodes {
    #[serde(default)]
    nodes: Vec<Node>,
}

#[derive(Debug, Default, Deserialize)]
struct ByIdsReply {
    #[serde(default)]
    share: Option<ByIdsShare>,
}

#[async_trait::async_trait]
impl Source for Client {
    async fn info(&self, cx: &CancellationToken) -> Result<Info, String> {
        let r: InfoReply = self
            .query(cx, "FioShare", INFO_QUERY, serde_json::json!({ "shareId": self.share_id }))
            .await?;
        let share = match r.share {
            Some(s) => s,
            None => return Err(ERR_INVALID_SHARE.to_string()),
        };
        if share.enabled == Some(false) {
            return Err(ERR_INVALID_SHARE.to_string());
        }
        Ok(Info {
            share_id: share.id,
            name: share.name.unwrap_or_default(),
            downloading_enabled: share.downloading_enabled.unwrap_or(true),
        })
    }

    /// List walks the share recursively and returns every file with its
    /// relative path.
    async fn list(
        &self,
        cx: &CancellationToken,
        progress: &(dyn Fn(usize) + Send + Sync),
    ) -> Result<Vec<File>, String> {
        let l = Lister::new(cx, progress);
        let mut files = self.walk(&l, None, String::new(), 0).await?;
        dedupe_paths(&mut files);
        Ok(files)
    }

    /// FreshURL re-queries a single asset's download URL (signed URLs expire).
    async fn fresh_url(&self, cx: &CancellationToken, asset: &str) -> Result<String, String> {
        let vars = serde_json::json!({
            "assetIds": [asset],
            "first": 1,
            "shareId": self.share_id,
        });
        let r: ByIdsReply = self.query(cx, "FioShareAssetsByIds", &BY_IDS_QUERY, vars).await?;
        if let Some(assets) = r.share.and_then(|s| s.assets) {
            for n in &assets.nodes {
                let (u, _) = n.head().url();
                if !u.is_empty() {
                    return Ok(u);
                }
            }
        }
        Err("could not refresh the download link for this file".to_string())
    }

    /// MarkDownloaded records the download in the share's activity, as the web
    /// page does. Best effort: errors are ignored by callers.
    async fn mark_downloaded(&self, cx: &CancellationToken, ids: &[String]) -> Result<(), String> {
        let mut i = 0;
        while i < ids.len() {
            let j = std::cmp::min(i + 100, ids.len());
            let vars = serde_json::json!({ "input": { "assetIds": ids[i..j] } });
            let _: serde_json::Value = self.query(cx, "MarkSharedAssetsDownloaded", MARK_MUTATION, vars).await?;
            i += 100;
        }
        Ok(())
    }
}

impl Client {
    fn walk<'a>(
        &'a self,
        l: &'a Lister<'_>,
        folder_id: Option<String>,
        prefix: String,
        depth: usize,
    ) -> BoxFuture<'a, Result<Vec<File>, String>> {
        Box::pin(async move {
            if depth > 64 {
                let e = "folder nesting too deep".to_string();
                l.fail(&e);
                return Err(e);
            }
            let nodes = self.folder_nodes(l, folder_id.as_deref()).await?;
            let mut parts: Vec<Vec<File>> = vec![Vec::new(); nodes.len()];
            let mut folders: Vec<usize> = Vec::new();
            for (i, n) in nodes.iter().enumerate() {
                if n.typename == "FolderAsset" {
                    folders.push(i);
                    continue;
                }
                let h = n.head();
                let (u, size) = h.url();
                let mut file_name = safe_name(&n.name);
                if !std::ptr::eq(h, n) && !h.name.is_empty() {
                    file_name = safe_name(&h.name);
                }
                let mut f = File {
                    asset_id: h.id.clone(),
                    rel_path: path_join(&prefix, &file_name),
                    size,
                    url: u,
                    problem: String::new(),
                };
                if f.url.is_empty() {
                    f.problem = "original not available".to_string();
                }
                parts[i] = vec![f];
            }
            l.found(nodes.len() - folders.len());
            let futs = folders.iter().map(|&i| {
                let id = nodes[i].id.clone();
                let sub = path_join(&prefix, &safe_name(&nodes[i].name));
                async move { (i, self.walk(l, Some(id), sub, depth + 1).await) }
            });
            let results = futures::future::join_all(futs).await;
            if let Some(c) = l.cause() {
                return Err(c);
            }
            for (i, r) in results {
                parts[i] = r?;
            }
            Ok(parts.into_iter().flatten().collect())
        })
    }

    /// folderNodes fetches every page of one folder's listing (None = share root).
    async fn folder_nodes(&self, l: &Lister<'_>, folder_id: Option<&str>) -> Result<Vec<Node>, String> {
        let mut all: Vec<Node> = Vec::new();
        let mut after: Option<String> = None;
        loop {
            let mut page = serde_json::Map::new();
            if let Some(a) = &after {
                page.insert("after".to_string(), serde_json::Value::String(a.clone()));
            }
            page.insert("first".to_string(), serde_json::Value::Number(200.into()));
            let vars = serde_json::json!({
                "folderId": folder_id,
                "page": serde_json::Value::Object(page),
                "shareId": self.share_id,
            });
            let r: ListReply = l.call(self.query(l.token(), "FioShareAssets", &LIST_QUERY, vars)).await?;
            let assets = match r.share.and_then(|s| s.assets) {
                Some(a) => a,
                None => return Ok(all),
            };
            all.extend(assets.nodes);
            if !assets.page_info.has_next_page {
                return Ok(all);
            }
            match assets.page_info.end_cursor {
                Some(c) => after = Some(c),
                None => return Ok(all),
            }
        }
    }
}

const BASE64: base64::engine::general_purpose::GeneralPurpose = base64::engine::general_purpose::STANDARD;

#[cfg(test)]
mod tests {
    use super::*;

    const ID: &str = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d";

    #[test]
    fn head_picks_newest_version() {
        let mut n = Node { typename: "VersionStackAsset".to_string(), ..Default::default() };
        n.versions = Some(vec![
            Node { id: "v1".to_string(), version_number: Some(1), ..Default::default() },
            Node { id: "v3".to_string(), version_number: Some(3), ..Default::default() },
            Node { id: "v2".to_string(), version_number: Some(2), ..Default::default() },
        ]);
        assert_eq!(n.head().id, "v3");
    }

    #[test]
    fn dedupe_and_safe_names() {
        let mut files = vec![
            File { rel_path: "a/clip.mov".to_string(), ..Default::default() },
            File { rel_path: "a/clip.mov".to_string(), ..Default::default() },
            File { rel_path: "a/CLIP.mov".to_string(), ..Default::default() },
            File { rel_path: "b.mov".to_string(), ..Default::default() },
        ];
        dedupe_paths(&mut files);
        let want = ["a/clip.mov", "a/clip (2).mov", "a/CLIP (3).mov", "b.mov"];
        for (i, w) in want.iter().enumerate() {
            assert_eq!(files[i].rel_path, *w, "file {}", i);
        }
        // A generated name must not take a name another file already has.
        let mut files = vec![
            File { rel_path: "a.mov".to_string(), ..Default::default() },
            File { rel_path: "a.mov".to_string(), ..Default::default() },
            File { rel_path: "a (2).mov".to_string(), ..Default::default() },
        ];
        dedupe_paths(&mut files);
        let want = ["a.mov", "a (3).mov", "a (2).mov"];
        for (i, w) in want.iter().enumerate() {
            assert_eq!(files[i].rel_path, *w, "collision case: file {}", i);
        }
        for (input, w) in [
            ("..", "untitled"),
            ("a/b", "a_b"),
            ("x:y", "x_y"),
            ("  ", "untitled"),
            ("ok.mov", "ok.mov"),
        ] {
            assert_eq!(safe_name(input), w, "SafeName({:?})", input);
        }
    }

    #[test]
    fn rel_paths() {
        assert_eq!(rel_path("Day 1/a.mov"), "Day 1/a.mov");
        assert_eq!(rel_path("../../etc/x.mov"), "etc/x.mov");
        assert_eq!(rel_path("c:d.wav"), "c_d.wav");
        assert_eq!(rel_path(""), "untitled");
    }

    #[test]
    fn looks_like_a_link() {
        let yes = [
            format!("https://next.frame.io/share/{}/", ID),
            format!("app.frame.io/reviews/{}", ID),
            "  https://f.io/AbCd12  ".to_string(),
            "https://fromsmash.com/AbCdEf-dt".to_string(),
            "https://we.tl/t-abc123".to_string(),
            "https://wetransfer.com/downloads/abc123/def456".to_string(),
            "https://www.dropbox.com/scl/fo/key/hash?rlkey=x".to_string(),
            "https://drive.google.com/drive/folders/1AbCdEfGhIjKlMnOpQrStUvWxYz".to_string(),
            "https://portal.example.com/d/AbCd1234efgh".to_string(),
        ];
        for s in yes {
            assert!(looks_like_link(&s), "LooksLikeLink({:?}) = false", s);
        }
        let no = [
            "",
            "hello",
            "https://f.io/",
            "https://fromsmash.com/",
            "https://example.com/reset?token=abc",
            "https://example.com/d/x",
            "https://f.io/a and more",
            ID,
            "https://www.dropbox.com/home",
        ];
        for s in no {
            assert!(!looks_like_link(s), "LooksLikeLink({:?}) = true", s);
        }
    }

    #[test]
    fn recognise_links() {
        let cases: [(&str, &str, &str, &str); 11] = [
            ("https://wetransfer.com/downloads/0123abcd20261023/9f8e7d", KIND_WETRANSFER, "0123abcd20261023", "https://wetransfer.com/downloads/0123abcd20261023/9f8e7d"),
            ("wetransfer.com/downloads/0123abcd20261023/r3c1p/9f8e7d?t_exp=1", KIND_WETRANSFER, "0123abcd20261023", "https://wetransfer.com/downloads/0123abcd20261023/r3c1p/9f8e7d"),
            ("https://studio.wetransfer.com/downloads/0123abcd20261023/9f8e7d", KIND_WETRANSFER, "0123abcd20261023", "https://wetransfer.com/downloads/0123abcd20261023/9f8e7d"),
            ("https://www.dropbox.com/scl/fo/k3y/h4sh?rlkey=rl&st=x&dl=0", KIND_DROPBOX, "k3y", "https://www.dropbox.com/scl/fo/k3y/h4sh?rlkey=rl"),
            ("https://www.dropbox.com/scl/fi/k3y/clip.mov?rlkey=rl&dl=0", KIND_DROPBOX, "k3y", "https://www.dropbox.com/scl/fi/k3y/clip.mov?rlkey=rl"),
            ("dropbox.com/sh/k3y/h4sh?dl=0", KIND_DROPBOX, "k3y", "https://www.dropbox.com/sh/k3y/h4sh"),
            ("https://www.dropbox.com/s/k3y/clip.mov?dl=0", KIND_DROPBOX, "k3y", "https://www.dropbox.com/s/k3y/clip.mov"),
            ("https://drive.google.com/drive/folders/1dQ4sx0-__Nvg65rxTSgQrl7VyW?usp=sharing", KIND_DRIVE, "1dQ4sx0-__Nvg65rxTSgQrl7VyW", "https://drive.google.com/open?id=1dQ4sx0-__Nvg65rxTSgQrl7VyW"),
            ("https://drive.google.com/drive/u/1/folders/0B2SJp-WVjVPrb25N?resourcekey=0-G_0d", KIND_DRIVE, "0B2SJp-WVjVPrb25N", "https://drive.google.com/open?id=0B2SJp-WVjVPrb25N&resourcekey=0-G_0d"),
            ("https://drive.google.com/file/d/1Jp0I0tS-qMxtXNe/view?usp=drive_link", KIND_DRIVE, "1Jp0I0tS-qMxtXNe", "https://drive.google.com/open?id=1Jp0I0tS-qMxtXNe"),
            ("https://drive.google.com/open?id=1Jp0I0tS-qMxtXNe", KIND_DRIVE, "1Jp0I0tS-qMxtXNe", "https://drive.google.com/open?id=1Jp0I0tS-qMxtXNe"),
        ];
        for (input, kind, id, url) in cases {
            let got = link_from_url(input);
            assert_eq!(
                got,
                Some(Link { kind: kind.to_string(), id: id.to_string(), url: url.to_string() }),
                "linkFromURL({:?})",
                input
            );
        }
        for input in [
            "https://wetransfer.com/",
            "https://wetransfer.com/downloads/x",
            "https://www.dropbox.com/home",
            "https://drive.google.com/drive/my-drive",
            "https://example.com/downloads/a/b",
        ] {
            assert_eq!(link_from_url(input), None, "linkFromURL({:?}) should not match", input);
        }
    }

    #[test]
    fn share_ids_and_labels() {
        for input in [
            format!("https://next.frame.io/share/{}/", ID),
            format!("https://next.frame.io/share/{}", ID),
            format!("next.frame.io/share/{}/view/11111111-2222-4333-8444-555555555555", ID),
            format!("https://next.frame.io/share/{}?x=1", ID.to_uppercase()),
        ] {
            let l = link_from_url(input.trim()).expect("share link");
            assert_eq!(l.kind, KIND_SHARE);
            assert_eq!(l.id, ID);
        }
        // A bare share ID is a link too: resolve_link lowercases it.
        assert!(is_uuid(ID));
        assert_eq!(ID.to_uppercase().len(), 36);
        assert!(!is_uuid("hello"));
        assert_eq!(
            Link { kind: KIND_SMASH.to_string(), id: "AbCdEf-dt".to_string(), url: String::new() }.label(),
            "Smash AbCdEf-dt"
        );
        assert_eq!(
            Link { kind: KIND_SHARE.to_string(), id: ID.to_string(), url: String::new() }.label(),
            "Frame.io share 0a1b2c3d"
        );
    }

    #[test]
    fn rfc3339_timestamps() {
        assert_eq!(parse_rfc3339("1970-01-01T00:00:00Z"), Some(0));
        assert_eq!(parse_rfc3339("2026-10-23T12:00:00Z"), Some(1792756800));
        assert_eq!(parse_rfc3339("2026-10-23T14:00:00+02:00"), Some(1792756800));
        assert_eq!(parse_rfc3339("2026-10-23T12:00:00.123Z"), Some(1792756800));
        assert_eq!(parse_rfc3339(""), None);
        assert_eq!(parse_rfc3339("2026-10-23"), None);
        assert!(now_unix() > 1_700_000_000);
    }

    #[test]
    fn smash_regions_from_the_id() {
        assert_eq!(smash::smash_region("AbCdEf-dt"), "eu-central-1");
        assert_eq!(smash::smash_region("AbCdEf"), "");
        assert_eq!(smash::smash_region("ab/d"), "");
        assert_eq!(smash::smash_target("https://fromsmash.com/grade-v3"), Some("fromsmash.com/grade-v3".to_string()));
        assert_eq!(smash::smash_target("https://ACME.fromsmash.com/xyz"), Some("acme.fromsmash.com/xyz".to_string()));
        assert_eq!(smash::smash_target("https://fromsmash.com/"), Some(String::new()));
        assert_eq!(smash::smash_target("https://example.com/x"), None);
    }
}
