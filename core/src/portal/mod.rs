//! Portal: the service that publishes a project's upload and download links,
//! and the `.turbosort` marker that names its `Live_Work` folder.
//!
//! FioTransfer talks to Portal for one reason: to learn which project folders
//! exist and which Frame.io folder each one publishes into, so that
//! [`agent::reconcile`] can keep a mirrored watch folder for every one of them.
//! No file passes through Portal. Transfers still go to and from Frame.io; the
//! local side is ours (`Settings::portal_root`), and Portal's own destination
//! paths are never adopted.
//!
//! Only two endpoints are used, both documented here and neither invented:
//! `GET /api/upload-links` (the links, each with its `target_folder_id`,
//! `target_folder_name`, `folder` and `turbosort`) and `GET /api/sync-rules`
//! (read-only reference: what Portal's own worker would do). Both are read with
//! a bearer token: `Authorization: Bearer <PORTAL_API_TOKEN>`.
//!
//! Tolerance: Portal is a live service and its rows may grow fields or send
//! `null` where a string is expected. `#[serde(default)]` alone does not cover
//! a present `null` (the Space property sweep found the same thing for
//! Frame.io), so every string field also goes through [`nullable`].

pub mod agent;

use std::path::PathBuf;
use std::sync::OnceLock;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::secret;

/// The Keychain item the bearer token lives under.
pub const TOKEN_KEY: &str = "portal_token";

/// The 0600 file that stands in where there is no Keychain, exactly as `auth`
/// keeps the Adobe session's `token.json`. It is never written to settings.json.
const TOKEN_FILE: &str = "portal-token";

/// Portal answers within this or is treated as unreachable.
pub const TIMEOUT: Duration = Duration::from_secs(30);

/// How many times one GET is tried before the error is reported.
pub const MAX_ATTEMPTS: u32 = 6;

// ---------- errors ----------

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum PortalErr {
    /// `Settings::portal_url` is empty: nothing to talk to.
    NoUrl,
    /// No bearer token has been stored.
    NoToken,
    /// Portal answered, and the answer was not a success.
    Status { status: u16, body: String },
    /// The connection failed, or failed and stayed failed.
    Http(String),
    /// The reply was not the JSON document this module knows.
    Json(String),
}

/// The human part of a Portal error body: `{"detail": "…"}` when it is that shape,
/// otherwise the body trimmed. Never the whole JSON document.
fn detail_of(body: &str) -> String {
    let t = body.trim();
    if t.is_empty() {
        return String::new();
    }
    if let Ok(v) = serde_json::from_str::<serde_json::Value>(t) {
        if let Some(d) = v.get("detail").and_then(|d| d.as_str()) {
            return d.trim().to_string();
        }
    }
    crate::netx::trim_body(t.as_bytes())
}

impl std::fmt::Display for PortalErr {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            PortalErr::NoUrl => write!(f, "no Portal URL is set"),
            PortalErr::NoToken => write!(f, "no Portal token is stored"),
            PortalErr::Status { status, body } => {
                // The user reads this in the settings status line, so it says what
                // to do rather than repeating a JSON error document.
                match *status {
                    401 | 403 => write!(
                        f,
                        "Portal refused the token ({}). Paste the PORTAL_API_TOKEN from Portal's .env and press Save",
                        status
                    ),
                    404 => write!(f, "Portal has no /api/upload-links ({}): is that the Portal address?", status),
                    _ => {
                        let body = detail_of(body);
                        if body.is_empty() {
                            write!(f, "Portal answered {} (no detail)", status)
                        } else {
                            write!(f, "Portal answered {}: {}", status, body)
                        }
                    }
                }
            }
            PortalErr::Http(e) => write!(f, "can't reach Portal: {}", e),
            PortalErr::Json(e) => write!(f, "Portal sent something else: {}", e),
        }
    }
}
impl std::error::Error for PortalErr {}

// ---------- the token ----------

fn token_path() -> PathBuf {
    crate::config::dir().join(TOKEN_FILE)
}

/// The stored bearer token, if there is one.
pub fn token() -> Option<String> {
    let raw = match secret::get(TOKEN_KEY) {
        Some(s) => s,
        None => std::fs::read_to_string(token_path()).ok()?,
    };
    let t = raw.trim().to_string();
    if t.is_empty() {
        None
    } else {
        Some(t)
    }
}

/// Whether a token is stored: what `portal_status`'s `signed_in` answers.
pub fn signed_in() -> bool {
    token().is_some()
}

/// Stores the bearer token. The Keychain is asked first; where it is absent (or
/// disabled for a headless run) a 0600 file under the app's own data folder
/// stands in, as it does for the Adobe session.
pub fn set_token(t: &str) -> Result<(), String> {
    let t = t.trim();
    if t.is_empty() {
        return Err("no token".to_string());
    }
    if secret::set(TOKEN_KEY, t).is_ok() {
        let _ = std::fs::remove_file(token_path());
        return Ok(());
    }
    use std::io::Write;
    let p = token_path();
    let mut opts = std::fs::OpenOptions::new();
    opts.create(true).write(true).truncate(true);
    // 0600 on unix; on Windows the folder's ACL is what keeps this token to its
    // owner (see `fileio::mode`).
    crate::fileio::mode(&mut opts, 0o600);
    let mut f = opts.open(&p).map_err(|e| format!("could not store the Portal token: {}", e))?;
    f.write_all(t.as_bytes()).map_err(|e| format!("could not store the Portal token: {}", e))
}

/// Forgets the bearer token.
pub fn clear_token() {
    secret::delete(TOKEN_KEY);
    let _ = std::fs::remove_file(token_path());
}

// ---------- tolerant reading ----------

/// `null` where a value is expected reads as the value's default. This is what
/// `#[serde(default)]` does not do: it covers an absent key, not a present one
/// that is `null`.
pub fn nullable<'de, D, T>(d: D) -> Result<T, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Deserialize<'de> + Default,
{
    Ok(Option::<T>::deserialize(d)?.unwrap_or_default())
}

fn text_at(v: &Value, key: &str) -> String {
    match v.get(key) {
        Some(Value::String(s)) => s.trim().to_string(),
        Some(Value::Number(n)) => n.to_string(),
        _ => String::new(),
    }
}

/// A Frame.io folder as Portal names it: `{"id": …, "name": …, "account_id":
/// …}`, or just its name as a string, or nothing at all.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize)]
pub struct Folder {
    pub id: String,
    pub name: String,
    pub account_id: String,
}

impl<'de> Deserialize<'de> for Folder {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Folder, D::Error> {
        let v = Value::deserialize(d)?;
        Ok(match &v {
            Value::String(s) => Folder { id: String::new(), name: s.trim().to_string(), account_id: String::new() },
            Value::Object(_) => Folder {
                id: text_at(&v, "id"),
                name: text_at(&v, "name"),
                account_id: {
                    let a = text_at(&v, "account_id");
                    if a.is_empty() {
                        text_at(&v, "target_account_id")
                    } else {
                        a
                    }
                },
            },
            _ => Folder::default(),
        })
    }
}

/// One row of `GET /api/upload-links`.
///
/// Only the fields the brief and the service name are modelled; every one is
/// optional on the wire, and unknown fields are ignored (serde's own default),
/// so a row that grows a field keeps working.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct UploadLink {
    #[serde(default, deserialize_with = "nullable")]
    pub id: String,
    /// A display name, when Portal has one.
    #[serde(default, deserialize_with = "nullable")]
    pub name: String,
    /// The same, under the other word Portal uses for it.
    #[serde(default, deserialize_with = "nullable")]
    pub title: String,
    #[serde(default, deserialize_with = "nullable")]
    pub target_folder_id: String,
    #[serde(default, deserialize_with = "nullable")]
    pub target_folder_name: String,
    /// The Frame.io account the target folder is in. Not named in the brief; a
    /// link that does not carry it (here, or on `folder`) cannot be watched, and
    /// the reconciler says so instead of inventing one.
    #[serde(default, deserialize_with = "nullable")]
    pub target_account_id: String,
    #[serde(default, deserialize_with = "nullable")]
    pub account_id: String,
    /// The `.turbosort` marker: the `Live_Work` folder this project belongs to.
    #[serde(default, deserialize_with = "nullable")]
    pub turbosort: String,
    /// The Frame.io folder, as an object or as a bare name.
    #[serde(default)]
    pub folder: Folder,
    /// Portal's word for a link that is not live.
    #[serde(default, deserialize_with = "nullable")]
    pub status: String,
    #[serde(default, deserialize_with = "nullable")]
    pub disabled: bool,
    /// The same flag the other way round; absent means "not said".
    #[serde(default)]
    pub enabled: Option<bool>,
}

/// The statuses that mean a link is not live. Portal's exact vocabulary is not
/// documented in anything this crate can see, so the words a row may carry are
/// listed here rather than guessed at one call site.
const OFF_STATUSES: [&str; 6] = ["disabled", "inactive", "archived", "deleted", "expired", "paused"];

impl UploadLink {
    /// The marker, trimmed. Empty means Portal is not asking for a watch.
    pub fn marker(&self) -> String {
        self.turbosort.trim().to_string()
    }

    /// The folder name to show on the watch row: the first name Portal gave it.
    pub fn label(&self) -> String {
        for s in [&self.name, &self.title, &self.target_folder_name, &self.folder.name, &self.target_folder_id, &self.id] {
            if !s.is_empty() {
                return s.clone();
            }
        }
        String::new()
    }

    /// The Frame.io account. Empty when the row does not say.
    pub fn acct(&self) -> String {
        for s in [&self.target_account_id, &self.account_id, &self.folder.account_id] {
            if !s.is_empty() {
                return s.clone();
            }
        }
        String::new()
    }

    /// The Frame.io folder the link publishes into. Empty when the row does not
    /// say.
    pub fn dest_folder(&self) -> String {
        for s in [&self.target_folder_id, &self.folder.id] {
            if !s.is_empty() {
                return s.clone();
            }
        }
        String::new()
    }

    /// Whether Portal says this link is not live: its watch goes.
    pub fn is_disabled(&self) -> bool {
        if self.disabled || self.enabled == Some(false) {
            return true;
        }
        let st = self.status.trim().to_ascii_lowercase();
        !st.is_empty() && OFF_STATUSES.contains(&st.as_str())
    }
}

/// One row of `GET /api/sync-rules`.
///
/// Read-only reference: it says what Portal's own worker would do with a
/// marker, including a `destination_path` and `path_template` under *its* disk.
/// None of it is adopted — the local root is a FioTransfer setting — so these
/// rows are for showing, not for acting on.
#[derive(Debug, Clone, Default, PartialEq, Serialize, Deserialize)]
pub struct SyncRule {
    #[serde(default, deserialize_with = "nullable")]
    pub id: String,
    #[serde(default, deserialize_with = "nullable")]
    pub name: String,
    #[serde(default, deserialize_with = "nullable")]
    pub turbosort: String,
    #[serde(default, deserialize_with = "nullable")]
    pub destination_path: String,
    #[serde(default, deserialize_with = "nullable")]
    pub path_template: String,
    #[serde(default, deserialize_with = "nullable")]
    pub target_folder_id: String,
    #[serde(default, deserialize_with = "nullable")]
    pub target_folder_name: String,
}

// ---------- the client ----------

fn http() -> &'static reqwest::Client {
    static C: OnceLock<reqwest::Client> = OnceLock::new();
    C.get_or_init(|| reqwest::Client::builder().timeout(TIMEOUT).build().expect("portal client"))
}

/// Portal's two read endpoints, over https, with the bearer token.
#[derive(Clone)]
pub struct PortalClient {
    base: String,
    token: String,
}

impl PortalClient {
    /// `base_url` is used as given, less a trailing slash, so `base + "/api/…"`
    /// has exactly one separator.
    pub fn new(base_url: &str, token: &str) -> PortalClient {
        PortalClient {
            base: base_url.trim().trim_end_matches('/').to_string(),
            token: token.trim().to_string(),
        }
    }

    /// The client the app builds: the base URL from the settings, the bearer
    /// token from the Keychain (or its 0600 fallback). `NoUrl` / `NoToken` when
    /// either is missing, so the caller never calls Portal half-configured.
    pub fn from_settings(s: &crate::config::Settings) -> Result<PortalClient, PortalErr> {
        if s.portal_url.trim().is_empty() {
            return Err(PortalErr::NoUrl);
        }
        let tok = token().ok_or(PortalErr::NoToken)?;
        Ok(PortalClient::new(&s.portal_url, &tok))
    }

    /// The URL one call goes to.
    pub fn url(&self, path: &str) -> String {
        format!("{}{}", self.base, path)
    }

    /// Who the token belongs to, when Portal says.
    pub async fn upload_links(&self) -> Result<Vec<UploadLink>, PortalErr> {
        let raw = self.get("/api/upload-links").await?;
        let rows = rows(&raw, "upload_links")?;
        let mut out: Vec<UploadLink> = Vec::with_capacity(rows.len());
        for r in rows {
            out.push(serde_json::from_value(r).map_err(|e| PortalErr::Json(e.to_string()))?);
        }
        Ok(out)
    }

    /// Portal's own sync rules. Read-only: nothing here acts on them.
    pub async fn sync_rules(&self) -> Result<Vec<SyncRule>, PortalErr> {
        let raw = self.get("/api/sync-rules").await?;
        let rows = rows(&raw, "sync_rules")?;
        let mut out: Vec<SyncRule> = Vec::with_capacity(rows.len());
        for r in rows {
            out.push(serde_json::from_value(r).map_err(|e| PortalErr::Json(e.to_string()))?);
        }
        Ok(out)
    }

    /// One GET, retried the way `api::Client` retries: exponential backoff from
    /// `netx::backoff`, a `Retry-After` honoured when Portal sends one, a
    /// connect error or timeout repeated (a GET is safe to repeat), and 429 or a
    /// server error repeated. 4xx is the answer: it is not tried again.
    async fn get(&self, path: &str) -> Result<Vec<u8>, PortalErr> {
        if self.base.is_empty() {
            return Err(PortalErr::NoUrl);
        }
        if self.token.is_empty() {
            return Err(PortalErr::NoToken);
        }
        let url = self.url(path);
        let mut attempt: u32 = 0;
        loop {
            let res = http()
                .get(&url)
                .header("Authorization", format!("Bearer {}", self.token))
                .header("Accept", "application/json")
                .send()
                .await;
            let resp = match res {
                Ok(r) => r,
                Err(e) => {
                    // A GET that never reached Portal can always be repeated; a
                    // 30 s timeout is the one case where it may have been read.
                    if attempt >= MAX_ATTEMPTS || !(e.is_connect() || e.is_timeout()) {
                        return Err(PortalErr::Http(e.to_string()));
                    }
                    let d = crate::netx::backoff(attempt);
                    attempt += 1;
                    tokio::time::sleep(d).await;
                    continue;
                }
            };
            let status = resp.status().as_u16();
            let retry_after = resp
                .headers()
                .get("Retry-After")
                .and_then(|v| v.to_str().ok())
                .and_then(|v| v.trim().parse::<u64>().ok());
            let data = resp.bytes().await.map(|b| b.to_vec()).unwrap_or_default();
            if status == 429 || status >= 500 {
                if attempt >= MAX_ATTEMPTS {
                    return Err(PortalErr::Status { status, body: crate::netx::trim_body(&data) });
                }
                let d = match retry_after {
                    Some(s) if s > 0 => Duration::from_secs(s),
                    _ => crate::netx::backoff(attempt),
                };
                attempt += 1;
                tokio::time::sleep(d).await;
                continue;
            }
            if status >= 300 {
                return Err(PortalErr::Status { status, body: crate::netx::trim_body(&data) });
            }
            return Ok(data);
        }
    }
}

/// The list inside a Portal reply.
///
/// A bare array, or an object that carries it under the endpoint's own name
/// (`upload_links`, `sync_rules`), or under `data` / `items` / `results` /
/// `rows`, one level down at most. `links` is deliberately not searched: it is
/// how Frame.io's envelopes carry pagination, and reading a cursor as a list
/// would be worse than saying the reply is not understood.
pub fn rows(raw: &[u8], key: &str) -> Result<Vec<Value>, PortalErr> {
    let v: Value = serde_json::from_slice(raw).map_err(|e| PortalErr::Json(e.to_string()))?;
    if let Value::Array(a) = &v {
        return Ok(a.clone());
    }
    let obj = match v.as_object() {
        Some(o) => o,
        None => return Err(PortalErr::Json("expected a list of rows".to_string())),
    };
    for k in [key, "data", "items", "results", "rows"] {
        match obj.get(k) {
            Some(Value::Array(a)) => return Ok(a.clone()),
            Some(Value::Object(inner)) => {
                if let Some(Value::Array(a)) = inner.get(key) {
                    return Ok(a.clone());
                }
                if k == "data" {
                    if let Some(Value::Array(a)) = inner.get("data") {
                        return Ok(a.clone());
                    }
                }
            }
            _ => {}
        }
    }
    Err(PortalErr::Json(format!("no {} list in the reply", key)))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_null_reads_as_the_default_and_unknown_fields_are_ignored() {
        let raw = r#"{"id":"L1","name":null,"target_folder_id":"F1","target_folder_name":"Reel",
                      "folder":{"id":"F1","name":null,"account_id":"A1"},"turbosort":"Rakete",
                      "something_new":{"a":1}}"#;
        let l: UploadLink = serde_json::from_str(raw).expect("tolerant read");
        assert_eq!(l.id, "L1");
        assert_eq!(l.name, "", "a null name is an empty name");
        assert_eq!(l.marker(), "Rakete");
        assert_eq!(l.acct(), "A1");
        assert_eq!(l.dest_folder(), "F1");
        assert_eq!(l.label(), "Reel", "the folder name when no link name survives");
    }

    #[test]
    fn a_folder_may_be_just_a_name() {
        let l: UploadLink = serde_json::from_str(r#"{"id":"L2","folder":"Projekt X","turbosort":"X"}"#).unwrap();
        assert_eq!(l.folder.name, "Projekt X");
        assert_eq!(l.label(), "Projekt X");
        assert_eq!(l.dest_folder(), "", "a bare name is not an id");
    }

    #[test]
    fn a_link_says_when_it_is_off() {
        let on: UploadLink = serde_json::from_str(r#"{"id":"L3"}"#).unwrap();
        assert!(!on.is_disabled());
        for raw in [r#"{"disabled":true}"#, r#"{"enabled":false}"#, r#"{"status":"Archived"}"#] {
            let l: UploadLink = serde_json::from_str(raw).unwrap();
            assert!(l.is_disabled(), "{} says the link is off", raw);
        }
        let l: UploadLink = serde_json::from_str(r#"{"enabled":true,"status":"live"}"#).unwrap();
        assert!(!l.is_disabled());
    }

    #[test]
    fn rows_are_found_in_every_shape_portal_is_known_to_send() {
        let one = r#"[{"id":"a"}]"#;
        assert_eq!(rows(one.as_bytes(), "upload_links").unwrap().len(), 1);
        for raw in [
            r#"{"upload_links":[{"id":"a"}]}"#,
            r#"{"data":[{"id":"a"}]}"#,
            r#"{"items":[{"id":"a"}]}"#,
            r#"{"data":{"upload_links":[{"id":"a"}]}}"#,
        ] {
            assert_eq!(rows(raw.as_bytes(), "upload_links").unwrap().len(), 1, "{}", raw);
        }
        // An empty list is a list, not an error.
        assert_eq!(rows(br#"{"upload_links":[]}"#, "upload_links").unwrap().len(), 0);
        // A cursor is not a list, and a row that is not an object is not a row.
        assert!(rows(br#"{"links":{"next":"/x"}}"#, "upload_links").is_err());
        assert!(rows(br#"{"data":{}}"#, "upload_links").is_err());
        assert!(rows(b"nope", "upload_links").is_err());
    }

    #[test]
    fn a_client_refuses_a_missing_url_before_it_touches_anything() {
        let s = crate::config::Settings::default();
        assert!(matches!(PortalClient::from_settings(&s), Err(PortalErr::NoUrl)));
    }

    #[test]
    fn a_client_joins_one_separator() {
        let c = PortalClient::new("https://portal.example.com/", " t ");
        assert_eq!(c.url("/api/upload-links"), "https://portal.example.com/api/upload-links");
        assert_eq!(c.token, "t");
    }
    #[test]
    fn an_error_reads_as_advice_not_as_a_json_document() {
        let refused = PortalErr::Status { status: 401, body: r#"{"detail":"Authentication required"}"#.into() };
        let text = refused.to_string();
        assert!(text.contains("refused the token"), "{text}");
        assert!(text.contains("401"), "{text}");
        assert!(!text.contains("detail"), "the JSON wrapper must not reach the user: {text}");

        let wrong_host = PortalErr::Status { status: 404, body: r#"{"detail":"Not Found"}"#.into() };
        assert!(wrong_host.to_string().contains("is that the Portal address"), "{}", wrong_host);

        let broken = PortalErr::Status { status: 502, body: r#"{"detail":"bad gateway upstream"}"#.into() };
        assert_eq!(broken.to_string(), "Portal answered 502: bad gateway upstream");

        let bare = PortalErr::Status { status: 503, body: "  ".into() };
        assert_eq!(bare.to_string(), "Portal answered 503 (no detail)");

        let html = PortalErr::Status { status: 500, body: "<html>oops</html>".into() };
        assert!(html.to_string().contains("oops"), "{}", html);
    }

}
