//! Asking GitHub for new releases, and installing one over this copy.
//!
//! A release must carry a "-macOS.zip" and a SHA256SUMS.txt (scripts/package.sh
//! makes both).

use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::io::Write;
use std::path::{Path, PathBuf};
use tokio_util::sync::CancellationToken;

/// The GitHub endpoint for the newest published release.
pub const DEFAULT_API: &str = "https://api.github.com/repos/farbhaus/FioTransfer/releases/latest";
/// The app's bundle identifier; an update must have the same one.
pub const BUNDLE_ID: &str = "com.farbhaus.fiotransfer";

const WORK_PREFIX: &str = ".FioTransfer-update-";

/// A published release with the files an update needs.
#[derive(Debug, Clone, Deserialize, Default)]
pub struct Release {
    /// "1.0.3"
    #[serde(default)]
    pub version: String,
    /// The release page.
    #[serde(default)]
    pub url: String,
    /// Download URL of the release's "-macOS.zip" asset.
    #[serde(default)]
    pub zip: String,
    /// The release's SHA256SUMS.txt asset URL (or, if a caller sets it, the
    /// expected 64-hex SHA-256 of the zip itself).
    #[serde(default)]
    pub sha256: String,
}

pub struct Source {
    pub api: String,
    pub user_agent: String,
}

impl Source {
    pub fn new(user_agent: &str) -> Self {
        Source { api: DEFAULT_API.to_string(), user_agent: user_agent.to_string() }
    }

    /// Returns the newest release. A release still missing its files (they are
    /// attached after it's created) comes back as None.
    pub async fn latest(&self, cx: &CancellationToken) -> Result<Option<Release>, String> {
        // A non-200 comes back as "GitHub answered HTTP %d" from get().
        let resp = get(cx, &self.api, &self.user_agent, Some("application/vnd.github+json")).await?;
        let body = read_capped(resp, 4 << 20).await?;
        #[derive(Deserialize)]
        struct Asset {
            #[serde(default)]
            name: String,
            #[serde(default)]
            browser_download_url: String,
        }
        #[derive(Deserialize)]
        struct Resp {
            #[serde(default)]
            tag_name: String,
            #[serde(default)]
            html_url: String,
            #[serde(default)]
            assets: Vec<Asset>,
        }
        let r: Resp = serde_json::from_slice(&body).map_err(|e| e.to_string())?;
        let mut rel = Release {
            version: r.tag_name.trim_start_matches('v').to_string(),
            url: r.html_url,
            zip: String::new(),
            sha256: String::new(),
        };
        for a in &r.assets {
            if a.name.ends_with("-macOS.zip") {
                rel.zip = a.browser_download_url.clone();
            } else if a.name == "SHA256SUMS.txt" {
                rel.sha256 = a.browser_download_url.clone();
            }
        }
        if parse_version(&rel.version).is_none() || rel.zip.is_empty() || rel.sha256.is_empty() {
            return Ok(None);
        }
        Ok(Some(rel))
    }

    /// Downloads `rel`, checks it, and swaps it in for the app at `app`. The
    /// running copy keeps working from its new place, the returned folder, which
    /// is to be deleted once it has quit. Nothing changes if any step fails.
    pub async fn install(
        &self,
        cx: &CancellationToken,
        rel: &Release,
        app: &Path,
        progress: &(dyn Fn(f64) + Send + Sync),
    ) -> Result<String, String> {
        let app_dir = parent(app).ok_or_else(|| "not running from an app bundle".to_string())?;
        // Same volume, so the swap is two renames.
        let work = mkdtemp(&app_dir, WORK_PREFIX)?;
        match self.install_in(cx, rel, app, &work, progress).await {
            Ok(()) => Ok(work.to_string_lossy().into_owned()),
            Err(e) => {
                let _ = std::fs::remove_dir_all(&work);
                Err(e)
            }
        }
    }

    async fn install_in(
        &self,
        cx: &CancellationToken,
        rel: &Release,
        app: &Path,
        work: &Path,
        progress: &(dyn Fn(f64) + Send + Sync),
    ) -> Result<(), String> {
        let want = self.checksum(cx, rel).await?;
        let zip_name = zip_name(rel);
        let zip = work.join(&zip_name);
        self.download(cx, &rel.zip, &zip, &want, progress).await?;

        let new = work.join("new");
        let out = std::process::Command::new("/usr/bin/ditto")
            .arg("-x")
            .arg("-k")
            .arg(&zip)
            .arg(&new)
            .output()
            .map_err(|e| e.to_string())?;
        if !out.status.success() {
            let mut msg = String::from_utf8_lossy(&out.stdout).to_string();
            msg.push_str(&String::from_utf8_lossy(&out.stderr));
            return Err(format!("unpacking the update: {}", msg.trim()));
        }
        let fresh = new.join("FioTransfer.app");
        check(&fresh, &rel.version, app)?;
        // A quarantined app can't be launched by the helper below.
        let _ = std::process::Command::new("/usr/bin/xattr")
            .arg("-dr")
            .arg("com.apple.quarantine")
            .arg(&fresh)
            .output();

        let prev = work.join("previous.app");
        std::fs::rename(app, &prev).map_err(|e| e.to_string())?;
        if let Err(e) = std::fs::rename(&fresh, app) {
            let _ = std::fs::rename(&prev, app);
            return Err(e.to_string());
        }
        Ok(())
    }

    /// Reads the zip's SHA-256 from the release's SHA256SUMS.txt.
    async fn checksum(&self, cx: &CancellationToken, rel: &Release) -> Result<String, String> {
        // A caller may hand us the expected sum itself instead of the sums URL.
        if is_sha256_hex(&rel.sha256) {
            return Ok(rel.sha256.to_lowercase());
        }
        let resp = get(cx, &rel.sha256, &self.user_agent, None).await?;
        let body = read_capped(resp, 1 << 20).await?;
        let text = String::from_utf8_lossy(&body);
        let name = zip_name(rel);
        for line in text.lines() {
            let line = line.trim();
            if let Some((sum, rest)) = line.split_once(' ') {
                if rest.trim_start_matches([' ', '*']) == name && sum.len() == 64 {
                    return Ok(sum.to_lowercase());
                }
            }
        }
        Err(format!("{} isn't listed in SHA256SUMS.txt", name))
    }

    async fn download(
        &self,
        cx: &CancellationToken,
        url: &str,
        dst: &Path,
        want: &str,
        progress: &(dyn Fn(f64) + Send + Sync),
    ) -> Result<(), String> {
        let mut resp = get(cx, url, &self.user_agent, None).await?;
        let total = resp.content_length().unwrap_or(0);
        let mut fh = std::fs::File::create(dst).map_err(|e| e.to_string())?;
        let mut h = Sha256::new();
        let mut done: i64 = 0;
        loop {
            let chunk = tokio::select! {
                c = resp.chunk() => c,
                _ = cx.cancelled() => return Err("context canceled".to_string()),
            };
            let Some(buf) = chunk.map_err(|e| format!("downloading the update: {}", e))? else {
                break;
            };
            if buf.is_empty() {
                continue;
            }
            fh.write_all(&buf).map_err(|e| e.to_string())?;
            h.update(&buf);
            done += buf.len() as i64;
            if total > 0 {
                progress(done as f64 / total as f64);
            }
        }
        let got = hex(&h.finalize());
        if got != want {
            return Err("the download doesn't match its checksum".to_string());
        }
        fh.flush().map_err(|e| e.to_string())?;
        drop(fh);
        Ok(())
    }
}

/// Whether `a` is a newer version than `b` ("1.0.10" > "1.0.9"). A leading "v"
/// and a "-suffix" are ignored; a version that doesn't parse (a development
/// build) is never newer or older than anything.
pub fn newer(a: &str, b: &str) -> bool {
    let (x, y) = match (parse_version(a), parse_version(b)) {
        (Some(x), Some(y)) => (x, y),
        _ => return false,
    };
    for i in 0..x.len().max(y.len()) {
        let p = x.get(i).copied().unwrap_or(0);
        let q = y.get(i).copied().unwrap_or(0);
        if p != q {
            return p > q;
        }
    }
    false
}

/// Whether this build is a released version (development builds don't update).
pub fn released(v: &str) -> bool {
    parse_version(v).is_some()
}

fn parse_version(v: &str) -> Option<Vec<i64>> {
    let v = v.strip_prefix('v').unwrap_or(v);
    let v = v.split('-').next().unwrap_or("");
    if v.is_empty() {
        return None;
    }
    let mut out: Vec<i64> = Vec::new();
    for p in v.split('.') {
        let n: i64 = p.parse().ok()?;
        if n < 0 {
            return None;
        }
        out.push(n);
    }
    Some(out)
}

/// The .app containing the running binary.
pub fn bundle_path(exe: &Path) -> Result<PathBuf, String> {
    let exe: PathBuf = std::fs::canonicalize(exe).unwrap_or_else(|_| exe.to_path_buf());
    // .app/Contents/MacOS/<exe>
    let app = match exe.parent().and_then(Path::parent).and_then(Path::parent) {
        Some(a) => a.to_path_buf(),
        None => return Err("not running from an app bundle".to_string()),
    };
    let in_macos = exe
        .parent()
        .and_then(Path::file_name)
        .map(|n| n == "MacOS")
        .unwrap_or(false);
    if app.extension().map(|e| e != "app").unwrap_or(true) || !in_macos {
        return Err("not running from an app bundle".to_string());
    }
    if app.to_string_lossy().contains("/AppTranslocation/") {
        return Err("move FioTransfer to Applications to update it here".to_string());
    }
    Ok(app)
}

/// Checks that the folder holding `app` can take a new copy.
pub fn can_replace(app: &Path) -> Result<(), String> {
    let dir = parent(app).unwrap_or_else(|| PathBuf::from("/"));
    // W_OK == 2, as Go's syscall.Access(dir, 2) asks (a path with an interior
    // NUL can't be asked about, and is not writable).
    if !fiotransfer_sys::platform::is_writable(&dir.to_string_lossy()) {
        return Err(format!("no permission to replace the app in {}", dir.display()));
    }
    Ok(())
}

/// Removes what earlier updates left next to `app`.
pub fn cleanup(app: &Path) {
    let dir = match parent(app) {
        Some(d) => d,
        None => return,
    };
    let entries = match std::fs::read_dir(&dir) {
        Ok(e) => e,
        Err(_) => return,
    };
    for e in entries.flatten() {
        if e.file_name().to_string_lossy().starts_with(WORK_PREFIX) {
            let _ = std::fs::remove_dir_all(e.path());
        }
    }
}

/// Starts a helper that waits (up to two minutes) for process `pid` to exit,
/// deletes `old` (the folder install returned) and opens `app`.
pub fn relaunch(pid: u32, app: &Path, old: &str) -> Result<(), String> {
    let base = Path::new(old).file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_default();
    if !base.starts_with(WORK_PREFIX) {
        return Err(format!("not an update folder: {}", old));
    }
    const SCRIPT: &str = r#"i=0
while kill -0 "$1" 2>/dev/null; do
  i=$((i + 1)); [ "$i" -gt 600 ] && exit 0
  sleep 0.2
done
case "$3" in */.FioTransfer-update-*) rm -rf "$3" ;; esac
open "$2""#;
    let mut cmd = std::process::Command::new("/bin/sh");
    cmd.arg("-c")
        .arg(SCRIPT)
        .arg("fiotransfer-relaunch")
        .arg(pid.to_string())
        .arg(app)
        .arg(old)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null());
    // Outlives the app: its own session, so quitting the app doesn't kill it.
    fiotransfer_sys::platform::pre_exec_detach(&mut cmd);
    let mut child = cmd.spawn().map_err(|e| e.to_string())?;
    // Reap the helper so the app doesn't leave a zombie behind while it quits.
    std::thread::spawn(move || {
        let _ = child.wait();
    });
    Ok(())
}

// ---------- the checks Install makes on the unpacked bundle ----------

/// Makes sure `fresh` is an intact FioTransfer of the expected version, signed
/// by the same team as the running copy when that one has a team.
fn check(fresh: &Path, version: &str, running: &Path) -> Result<(), String> {
    if std::fs::metadata(fresh).is_err() {
        return Err("the update doesn't contain FioTransfer.app".to_string());
    }
    let out = std::process::Command::new("/usr/bin/codesign")
        .arg("--verify")
        .arg("--deep")
        .arg("--strict")
        .arg(fresh)
        .output()
        .map_err(|e| e.to_string())?;
    if !out.status.success() {
        let mut msg = String::from_utf8_lossy(&out.stdout).to_string();
        msg.push_str(&String::from_utf8_lossy(&out.stderr));
        return Err(format!("the update's signature is invalid: {}", msg.trim()));
    }
    let plist = fresh.join("Contents").join("Info.plist");
    let id = plist_value(&plist, "CFBundleIdentifier");
    if id != BUNDLE_ID {
        return Err(format!("the update is \"{}\", not FioTransfer", id));
    }
    let v = plist_value(&plist, "CFBundleShortVersionString");
    if v != version {
        return Err(format!("the update is version \"{}\", expected {}", v, version));
    }
    let team = team_id(running);
    if !team.is_empty() && team_id(fresh) != team {
        return Err("the update isn't signed by the same developer".to_string());
    }
    Ok(())
}

fn plist_value(plist: &Path, key: &str) -> String {
    match std::process::Command::new("/usr/bin/plutil")
        .arg("-extract")
        .arg(key)
        .arg("raw")
        .arg("-o")
        .arg("-")
        .arg(plist)
        .output()
    {
        Ok(o) if o.status.success() => String::from_utf8_lossy(&o.stdout).trim().to_string(),
        _ => String::new(),
    }
}

/// The signing team of an app, "" when it's ad-hoc signed.
fn team_id(app: &Path) -> String {
    let out = match std::process::Command::new("/usr/bin/codesign").arg("-dv").arg(app).output() {
        Ok(o) => o,
        Err(_) => return String::new(),
    };
    let mut text = String::from_utf8_lossy(&out.stdout).to_string();
    text.push_str(&String::from_utf8_lossy(&out.stderr));
    for l in text.lines() {
        if let Some(t) = l.strip_prefix("TeamIdentifier=") {
            if t != "not set" {
                return t.trim().to_string();
            }
        }
    }
    String::new()
}

// ---------- small helpers ----------

fn parent(p: &Path) -> Option<PathBuf> {
    p.parent().filter(|d| !d.as_os_str().is_empty()).map(|d| d.to_path_buf())
}

/// The zip asset's file name, which SHA256SUMS.txt lists it under.
fn zip_name(rel: &Release) -> String {
    if let Some(last) = rel.zip.rsplit('/').next() {
        if last.ends_with("-macOS.zip") {
            return last.to_string();
        }
    }
    format!("FioTransfer-{}-macOS.zip", rel.version)
}

fn is_sha256_hex(s: &str) -> bool {
    s.len() == 64 && s.chars().all(|c| c.is_ascii_hexdigit())
}

fn hex(b: &[u8]) -> String {
    let mut s = String::with_capacity(b.len() * 2);
    for x in b {
        s.push_str(&format!("{:02x}", x));
    }
    s
}

/// A GET the way the Go client made it: user agent always, no redirect tricks,
/// and cancelled when `cx` is.
async fn get(
    cx: &CancellationToken,
    url: &str,
    user_agent: &str,
    accept: Option<&str>,
) -> Result<reqwest::Response, String> {
    let client = crate::netx::api_client();
    let mut req = client.get(url).header("User-Agent", user_agent);
    if let Some(a) = accept {
        req = req.header("Accept", a);
    }
    let resp = tokio::select! {
        r = req.send() => r.map_err(|e| e.to_string())?,
        _ = cx.cancelled() => return Err("context canceled".to_string()),
    };
    let status = resp.status().as_u16();
    if status != 200 {
        return Err(format!("GitHub answered HTTP {}", status));
    }
    Ok(resp)
}

/// Reads at most `cap` bytes of a response body.
async fn read_capped(mut resp: reqwest::Response, cap: usize) -> Result<Vec<u8>, String> {
    let mut out: Vec<u8> = Vec::new();
    while let Some(chunk) = resp.chunk().await.map_err(|e| e.to_string())? {
        let room = cap.saturating_sub(out.len());
        if room == 0 {
            break;
        }
        out.extend_from_slice(&chunk[..chunk.len().min(room)]);
    }
    Ok(out)
}

/// Go's os.MkdirTemp(dir, prefix): a fresh 0700 folder next to the app.
fn mkdtemp(dir: &Path, prefix: &str) -> Result<PathBuf, String> {
    let mut last = String::new();
    let seed = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos() as u64)
        .unwrap_or(0);
    for i in 0..100u64 {
        let n = seed ^ (i.wrapping_mul(0x9e37_79b9_7f4a_7c15)) ^ ((std::process::id() as u64) << 32);
        let p = dir.join(format!("{}{:012x}", prefix, n & 0xffff_ffff_ffff));
        let mut builder = std::fs::DirBuilder::new();
        // 0700 on unix; on Windows the folder inherits its parent's ACL (see
        // `fileio::dir_mode`).
        crate::fileio::dir_mode(&mut builder, 0o700);
        match builder.create(&p) {
            Ok(()) => return Ok(p),
            Err(e) => last = e.to_string(),
        }
    }
    Err(last)
}

#[cfg(test)]
mod tests {
    use super::*;

    // Go's TestNewer.
    #[test]
    fn versions_compare() {
        for (a, b, want) in [
            ("1.0.3", "1.0.2", true),
            ("v1.0.10", "1.0.9", true),
            ("1.1", "1.0.9", true),
            ("1.0.2", "1.0.2", false),
            ("1.0.2", "1.0.3", false),
            ("1.0.2", "1.0.2.0", false),
            ("2.0.0", "0.0.0-abc1234", true), // CI builds of main
            ("1.0.3", "dev", false),
            ("dev", "1.0.0", false),
            ("1.0.x", "1.0.0", false),
        ] {
            assert_eq!(newer(a, b), want, "newer({:?}, {:?})", a, b);
        }
    }

    // Go's TestBundlePath.
    #[test]
    fn bundle_paths() {
        let p = bundle_path(Path::new("/Applications/FioTransfer.app/Contents/MacOS/FioTransfer"));
        assert_eq!(p.unwrap(), PathBuf::from("/Applications/FioTransfer.app"));
        assert!(bundle_path(Path::new(
            "/private/var/folders/x/T/AppTranslocation/ABC/d/FioTransfer.app/Contents/MacOS/FioTransfer"
        ))
        .is_err());
        assert!(bundle_path(Path::new("/Users/x/go/bin/fio")).is_err());
    }

    // Go's TestRelaunchOnlyRemovesUpdateFolders: the helper deletes the folder
    // it's given, so only an update's own folder is accepted.
    #[test]
    fn relaunch_only_removes_update_folders() {
        for old in ["/Applications", "/Users/x", "", "/Applications/FioTransfer.app"] {
            assert!(relaunch(std::process::id(), Path::new("/Applications/FioTransfer.app"), old).is_err());
        }
    }

    #[test]
    fn released_versions() {
        assert!(released("1.0.3"));
        assert!(released("v1.0.3"));
        assert!(!released("dev"));
        assert!(!released(""));
        assert!(!released("1.x"));
    }
}
