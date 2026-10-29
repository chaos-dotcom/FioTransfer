//! Google Drive files and folders shared with anyone who has the link.

use serde::de::DeserializeOwned;
use serde::Deserialize;
use std::sync::Mutex;
use tokio_util::sync::CancellationToken;

use super::{
    dedupe_paths, encode_query, netx_sleep, parse_link, path_escape, path_join, path_parts, safe_name,
    Info, Link, Lister, Source, ERR_INVALID_SHARE, KIND_DRIVE,
};
use crate::netx;

// Google Drive endpoints (overridable for tests). The key is the Drive web
// app's own public API key; Google accepts it only with a drive.google.com
// referer.
pub fn drive_api() -> String {
    std::env::var("FIO_DRIVE_API").unwrap_or_else(|_| "https://clients6.google.com/drive/v2beta".to_string())
}
pub fn drive_download() -> String {
    std::env::var("FIO_DRIVE_DOWNLOAD")
        .unwrap_or_else(|_| "https://drive.usercontent.google.com/download".to_string())
}
pub fn drive_key() -> String {
    std::env::var("FIO_DRIVE_KEY").unwrap_or_else(|_| "AIzaSyC1qbk75NzWBvSaDh6KnsjjA9pIrP4lYIE".to_string())
}

const DRIVE_FOLDER: &str = "application/vnd.google-apps.folder";
/// Docs, Sheets, shortcuts…: no file to download.
const DRIVE_APPS: &str = "application/vnd.google-apps.";
const DRIVE_ITEM_SPEC: &str = "id,title,mimeType,fileSize,resourceKey";

/// driveIDRe is ^[A-Za-z0-9_-]{10,}$.
fn drive_id_ok(s: &str) -> bool {
    s.len() >= 10 && s.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
}

/// driveLink recognises drive.google.com/drive/[u/N/]folders/<id>, /file/d/<id>/…
/// and ?id=<id> links, keeping a resourcekey (older links need one).
pub(crate) fn drive_link(s: &str) -> Option<Link> {
    let u = parse_link(s)?;
    let host = u.host_str().unwrap_or("");
    if host != "drive.google.com" && host != "drive.usercontent.google.com" {
        return None;
    }
    let p = path_parts(&u);
    let mut id = u
        .query_pairs()
        .find(|(k, _)| k == "id")
        .map(|(_, v)| v.to_string())
        .unwrap_or_default();
    for i in 0..p.len().saturating_sub(1) {
        if p[i] == "folders" || (p[i] == "file" && p[i + 1] == "d" && i + 2 < p.len()) {
            id = if p[i] == "file" { p[i + 2].clone() } else { p[i + 1].clone() };
            break;
        }
    }
    if !drive_id_ok(&id) {
        return None;
    }
    let mut q = vec![("id".to_string(), id.clone())];
    if let Some((_, rk)) = u.query_pairs().find(|(k, _)| k == "resourcekey") {
        if !rk.is_empty() {
            q.push(("resourcekey".to_string(), rk.to_string()));
        }
    }
    Some(Link {
        kind: KIND_DRIVE.to_string(),
        id,
        url: format!("https://drive.google.com/open?{}", encode_query(&q)),
    })
}

/// set_q sets one query parameter, as Go's url.Values.Set does.
fn set_q(q: &mut Vec<(String, String)>, k: &str, v: String) {
    q.retain(|(name, _)| name != k);
    q.push((k.to_string(), v));
}

/// DriveClient reads a Drive file or folder shared with anyone who has the
/// link, through the API the Drive web page uses.
pub struct DriveClient {
    pub id: String,
    pub resource_key: String,

    /// From info, reused by list.
    root: Mutex<Option<DriveItem>>,
}

/// newDrive builds the client for a resolved link.
pub(crate) fn new_drive(l: &Link) -> DriveClient {
    let mut resource_key = String::new();
    if let Ok(u) = url::Url::parse(&l.url) {
        resource_key = u
            .query_pairs()
            .find(|(k, _)| k == "resourcekey")
            .map(|(_, v)| v.to_string())
            .unwrap_or_default();
    }
    DriveClient { id: l.id.clone(), resource_key, root: Mutex::new(None) }
}

#[derive(Debug, Clone, Default, Deserialize)]
struct DriveItem {
    #[serde(default)]
    id: String,
    #[serde(default)]
    title: String,
    #[serde(rename = "mimeType", default)]
    mime_type: String,
    #[serde(rename = "fileSize", default)]
    file_size: String,
    #[serde(rename = "resourceKey", default)]
    resource_key: String,
}

impl DriveItem {
    /// asset is the ID fresh_url gets back: the file ID, and its resource key
    /// if it has one.
    fn asset(&self) -> String {
        if self.resource_key.is_empty() {
            self.id.clone()
        } else {
            format!("{}/{}", self.id, self.resource_key)
        }
    }

    fn file(&self, prefix: &str) -> super::File {
        let size = self.file_size.parse::<i64>().unwrap_or(0);
        let mut f = super::File {
            asset_id: self.asset(),
            rel_path: path_join(prefix, &safe_name(&self.title)),
            size,
            url: String::new(),
            problem: String::new(),
        };
        if self.mime_type.starts_with(DRIVE_APPS) {
            f.problem = "Google Docs, Sheets and shortcuts aren't files".to_string();
        }
        f
    }
}

impl DriveClient {
    /// get calls the Drive API. keys is the "id/resourcekey" pair the call needs.
    async fn get<T: DeserializeOwned>(
        &self,
        cx: &CancellationToken,
        endpoint: &str,
        q: &mut Vec<(String, String)>,
        keys: &str,
    ) -> Result<T, String> {
        set_q(q, "key", drive_key());
        set_q(q, "supportsTeamDrives", "true".to_string());
        let u = format!("{}{}?{}", drive_api(), endpoint, encode_query(q));
        let mut attempt = 0usize;
        loop {
            let (status, data) = super::send(cx, netx::api_client(), "Google Drive", || {
                let mut rb = netx::api_client().get(&u).header("Referer", "https://drive.google.com/");
                if !keys.is_empty() {
                    rb = rb.header("X-Goog-Drive-Resource-Keys", keys);
                }
                rb
            })
            .await?;
            if status < 300 {
                return serde_json::from_slice(&data)
                    .map_err(|_| "unexpected response from Google Drive".to_string());
            }
            #[derive(Debug, Default, Deserialize)]
            struct Detail {
                #[serde(default)]
                message: String,
                #[serde(default)]
                errors: Vec<Reason>,
            }
            #[derive(Debug, Default, Deserialize)]
            struct Reason {
                #[serde(default)]
                reason: String,
            }
            #[derive(Debug, Default, Deserialize)]
            struct E {
                #[serde(default)]
                error: Detail,
            }
            let e: E = serde_json::from_slice(&data).unwrap_or_default();
            let limited = e.error.errors.first().map(|r| r.reason.contains("RateLimitExceeded")).unwrap_or(false);
            if status == 403 && limited && attempt < 6 {
                netx_sleep(cx, netx::backoff(attempt as u32 + 1)).await?;
                attempt += 1;
                continue;
            }
            if status == 404 {
                // also what a folder that isn't shared publicly looks like
                return Err(ERR_INVALID_SHARE.to_string());
            }
            return Err(format!("Google Drive returned {}: {}", status, e.error.message));
        }
    }

    fn root_keys(&self) -> String {
        if self.resource_key.is_empty() {
            return String::new();
        }
        format!("{}/{}", self.id, self.resource_key)
    }

    fn walk<'a>(
        &'a self,
        l: &'a Lister<'_>,
        folder: &'a DriveItem,
        prefix: String,
        depth: usize,
    ) -> futures::future::BoxFuture<'a, Result<Vec<super::File>, String>> {
        Box::pin(async move {
            if depth > 64 {
                let e = "folder nesting too deep".to_string();
                l.fail(&e);
                return Err(e);
            }
            let mut q: Vec<(String, String)> = vec![
                ("q".to_string(), format!("trashed = false and '{}' in parents", folder.id)),
                ("fields".to_string(), format!("nextPageToken,items({})", DRIVE_ITEM_SPEC)),
                ("maxResults".to_string(), "1000".to_string()),
                ("orderBy".to_string(), "folder,title_natural asc".to_string()),
                ("includeItemsFromAllDrives".to_string(), "true".to_string()),
            ];
            let keys = if folder.resource_key.is_empty() { String::new() } else { folder.asset() };
            let mut items: Vec<DriveItem> = Vec::new();
            for _ in 0..10000 {
                #[derive(Debug, Default, Deserialize)]
                struct R {
                    #[serde(rename = "nextPageToken", default)]
                    next_page_token: String,
                    #[serde(default)]
                    items: Vec<DriveItem>,
                }
                let r: R = l.call(self.get(l.token(), "/files", &mut q, &keys)).await?;
                items.extend(r.items);
                if r.next_page_token.is_empty() {
                    break;
                }
                set_q(&mut q, "pageToken", r.next_page_token);
            }
            let mut parts: Vec<Vec<super::File>> = vec![Vec::new(); items.len()];
            let mut folders: Vec<usize> = Vec::new();
            for (i, it) in items.iter().enumerate() {
                if it.mime_type == DRIVE_FOLDER {
                    folders.push(i);
                    continue;
                }
                parts[i] = vec![it.file(&prefix)];
            }
            l.found(items.len() - folders.len());
            let mut futs = Vec::with_capacity(folders.len());
            for &i in &folders {
                let sub = path_join(&prefix, &safe_name(&items[i].title));
                let it: &DriveItem = &items[i];
                futs.push(async move { (i, self.walk(l, it, sub, depth + 1).await) });
            }
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
}

#[async_trait::async_trait]
impl Source for DriveClient {
    async fn info(&self, cx: &CancellationToken) -> Result<Info, String> {
        let mut q = vec![("fields".to_string(), DRIVE_ITEM_SPEC.to_string())];
        let it: DriveItem = self
            .get(cx, &format!("/files/{}", path_escape(&self.id)), &mut q, &self.root_keys())
            .await?;
        let mut info = Info { share_id: self.id.clone(), name: String::new(), downloading_enabled: true };
        if it.mime_type == DRIVE_FOLDER {
            info.name = it.title.clone();
        }
        if let Ok(mut g) = self.root.lock() {
            *g = Some(it);
        }
        Ok(info)
    }

    /// List walks a shared folder and returns each file to download on its own
    /// (Drive would send the folder as zips). A shared file is a list of one.
    async fn list(
        &self,
        cx: &CancellationToken,
        progress: &(dyn Fn(usize) + Send + Sync),
    ) -> Result<Vec<super::File>, String> {
        let root = self.root.lock().ok().and_then(|g| g.clone());
        let root = match root {
            Some(r) => r,
            None => {
                self.info(cx).await?;
                match self.root.lock().ok().and_then(|g| g.clone()) {
                    Some(r) => r,
                    None => return Err("unexpected response from Google Drive".to_string()),
                }
            }
        };
        if root.mime_type != DRIVE_FOLDER {
            (progress)(1);
            return Ok(vec![root.file("")]);
        }
        let l = Lister::new(cx, progress);
        let mut files = self.walk(&l, &root, String::new(), 0).await?;
        dedupe_paths(&mut files);
        Ok(files)
    }

    /// FreshURL returns the file's download URL. confirm=t skips the page Drive
    /// shows instead of files too big to scan for viruses.
    async fn fresh_url(&self, _cx: &CancellationToken, asset: &str) -> Result<String, String> {
        let (id, rk) = match asset.split_once('/') {
            Some((a, b)) => (a, b),
            None => (asset, ""),
        };
        let mut q = vec![
            ("id".to_string(), id.to_string()),
            ("export".to_string(), "download".to_string()),
            ("confirm".to_string(), "t".to_string()),
        ];
        if !rk.is_empty() {
            q.push(("resourcekey".to_string(), rk.to_string()));
        }
        Ok(format!("{}?{}", drive_download(), encode_query(&q)))
    }

    /// MarkDownloaded is a no-op for Drive links.
    async fn mark_downloaded(&self, cx: &CancellationToken, _ids: &[String]) -> Result<(), String> {
        let _ = cx;
        Ok(())
    }
}
