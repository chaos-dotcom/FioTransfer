
//! `fio` — the command-line front end to the FioTransfer engine.
//!
//!   fio inspect <link> [-p passphrase]
//!   fio get <link> [-o dir] [-p passphrase] [-n streams] [--subfolder]
//!   fio login | logout | whoami
//!   fio ls [account-id [folder-id]]
//!   fio up <path>... --account <id> --folder <id>
//!   fio history [-n 20]

#![forbid(unsafe_code)]
// The terminal and signal questions are `fiotransfer-sys`'s; nothing here
// writes `unsafe`.

use std::io::Write;
use std::sync::Arc;
use std::time::{Duration, Instant};

use fiotransfer::api::Client;
use fiotransfer::auth::AuthManager;
use fiotransfer::config;
use fiotransfer::history;
use fiotransfer::limiter::human_bytes;
use fiotransfer::share;
use fiotransfer::xfer::{self, manager as xfer_manager, DownloadOptions, Status, UploadOptions};

fn usage() -> ! {
    eprintln!(
        "fio — fast Frame.io transfers

  fio inspect <link> [-p passphrase]          show what a share link contains
  fio get <link> [-o dir] [-p pass] [-n N]    download a share link (resumable)
        [--subfolder] [--seg MB]
  fio login | logout | whoami                 Adobe sign-in for uploads
  fio ls [account [folder]]                   browse your Frame.io accounts/projects/folders
  fio up <path>... --account ID --folder ID   upload files/folders
  fio history [-n 20]                         recent transfers"
    );
    std::process::exit(2);
}

fn fatal(msg: &str) -> ! {
    eprintln!("error: {}", msg);
    std::process::exit(1);
}

#[tokio::main]
async fn main() {
    let argv: Vec<String> = std::env::args().skip(1).collect();
    if argv.is_empty() {
        usage();
    }
    let cmd = argv[0].clone();
    let args = &argv[1..];

    let settings = config::load();
    let settings_cell = Arc::new(std::sync::Mutex::new(settings.clone()));
    let auth = {
        let c = settings_cell.clone();
        AuthManager::new(move || c.lock().unwrap().client_id.clone())
    };
    let client = Arc::new(Client::new(auth.clone()));
    {
        let c = client.clone();
        *share::VIEWER.lock().unwrap() = Some(Arc::new(move || None::<(String, String)>.or_else(|| {
            let _ = &c;
            None
        })));
    }

    match cmd.as_str() {
        "inspect" => {
            let (pass, link) = flags_and_positional(args, &["-p"]);
            let m = new_manager(client.clone(), settings.clone()).await;
            let cx = tokio_util::sync::CancellationToken::new();
            let li = m.inspect_link(&cx, &link, &pass).await;
            if !li.error.is_empty() {
                fatal(&li.error);
            }
            println!(
                "{} link {}\nName:   {}\nFiles:  {} ({} unavailable)\nSize:   {}",
                li.kind,
                li.share_id,
                li.name,
                li.files,
                li.unavailable,
                human_bytes(li.total_bytes)
            );
        }
        "get" => {
            let (mut opts, link) = parse_get_flags(args, &settings);
            if link.is_empty() {
                usage();
            }
            check_range("-n", opts.streams, 1, 128);
            check_range("--seg", opts.segment_mb, 8, 512);
            let mut s = settings.clone();
            s.streams = opts.streams as i32;
            s.segment_mb = opts.segment_mb as i32;
            let m = new_manager(client.clone(), s).await;
            if let Err(e) = std::fs::create_dir_all(&opts.dir) {
                fatal(&e.to_string());
            }
            let j = match m.start_download(DownloadOptions {
                link,
                passphrase: opts.pass,
                dest_dir: opts.dir,
                subfolder: opts.subfolder,
            }) {
                Ok(j) => j,
                Err(e) => fatal(&e),
            };
            let code = follow(m.clone(), &j.id).await;
            std::process::exit(code);
        }
        "login" => {
            if settings.client_id.is_empty() {
                fatal(config::ERR_NO_CLIENT_ID);
            }
            println!("Opening your browser for Adobe sign-in…");
            let cx = tokio_util::sync::CancellationToken::new();
            let open = |u: &str| {
                let _ = std::process::Command::new("open").arg(u).spawn();
            };
            if let Err(e) = auth.login(&cx, settings.redirect_port.max(1) as u16, open).await {
                fatal(&e);
            }
            match client.me(&cx).await {
                Ok(me) => println!("Signed in as {} <{}>", me.name, me.email),
                Err(e) => fatal(&e.to_string()),
            }
        }
        "logout" => {
            auth.logout();
            println!("Signed out.");
        }
        "whoami" => {
            let cx = tokio_util::sync::CancellationToken::new();
            match client.me(&cx).await {
                Ok(me) => println!("{} <{}>", me.name, me.email),
                Err(e) => fatal(&e.to_string()),
            }
        }
        "ls" => {
            let cx = tokio_util::sync::CancellationToken::new();
            match args.len() {
                0 => match client.accounts(&cx).await {
                    Ok(accts) => {
                        for a in accts {
                            println!("account {}  {}", a.id, a.display_name);
                        }
                    }
                    Err(e) => fatal(&e.to_string()),
                },
                1 => {
                    let acct = &args[0];
                    match client.workspaces(&cx, acct).await {
                        Ok(wss) => {
                            for w in wss {
                                if let Ok(ps) = client.projects(&cx, acct, &w.id).await {
                                    for p in ps {
                                        println!("{:<20} / {:<30} root folder {}", w.name, p.name, p.root_folder_id);
                                    }
                                }
                            }
                        }
                        Err(e) => fatal(&e.to_string()),
                    }
                }
                _ => match client.children(&cx, &args[0], &args[1]).await {
                    Ok(kids) => {
                        for k in kids {
                            println!("{:<13} {}  {}", k.type_, k.id, k.name);
                        }
                    }
                    Err(e) => fatal(&e.to_string()),
                },
            }
        }
        "up" => {
            let (paths, account, folder, streams) = parse_up_flags(args, &settings);
            if paths.is_empty() {
                usage();
            }
            check_range("-n", streams, 1, 128);
            let mut s = settings.clone();
            s.streams = streams as i32;
            let m = new_manager(client.clone(), s).await;
            let j = match m.start_upload(UploadOptions {
                paths,
                account_id: account.clone(),
                folder_id: folder.clone(),
                dest_label: format!("folder {}", folder),
            }) {
                Ok(j) => j,
                Err(e) => fatal(&e),
            };
            let code = follow(m.clone(), &j.id).await;
            std::process::exit(code);
        }
        "history" => {
            let (_, n) = flags_and_positional(args, &["-n"]);
            let limit: i64 = n.parse().unwrap_or(20);
            let h = match history::Store::open(&config::dir(), &config::log_dir()) {
                Ok(h) => h,
                Err(e) => fatal(&e),
            };
            match h.list("", "", limit) {
                Ok(rows) => {
                    for r in rows {
                        println!(
                            "{}  {:<8} {:<9} {:4}/{:<4} {:>10}  {:>8}/s  {}",
                            fmt_ms(r.created_at),
                            r.kind,
                            r.status,
                            r.files_done,
                            r.files_total,
                            human_bytes(r.done_bytes),
                            human_bytes(r.avg_bps as i64),
                            r.title
                        );
                    }
                }
                Err(e) => fatal(&e),
            }
            println!("Log: {}", h.log_path());
        }
        _ => usage(),
    }
}

async fn new_manager(client: Arc<Client>, settings: config::Settings) -> Arc<xfer::Manager> {
    let h = match history::Store::open(&config::dir(), &config::log_dir()) {
        Ok(h) => Arc::new(h),
        Err(e) => fatal(&e),
    };
    let cell = Arc::new(std::sync::Mutex::new(settings));
    let m = xfer_manager::new_manager(client, h, Box::new(move || cell.lock().unwrap().clone()));
    m.set_keep_awake(true);
    m
}

fn check_range(flag: &str, v: i64, lo: i64, hi: i64) {
    if let Err(e) = range_check(flag, v, lo, hi) {
        fatal(&e);
    }
}

/// The message `fio` prints for an out-of-range number, or `Ok` when the value
/// is inside `lo..=hi`. (Separate from `check_range` so tests can see it:
/// `check_range` ends the process.)
fn range_check(flag: &str, v: i64, lo: i64, hi: i64) -> Result<(), String> {
    if v < lo || v > hi {
        return Err(format!("{} must be between {} and {}", flag, lo, hi));
    }
    Ok(())
}

/// Keeps the flags the command understands, and the single positional argument.
fn flags_and_positional(args: &[String], flags: &[&str]) -> (String, String) {
    let mut flag_vals: Vec<String> = Vec::new();
    let mut pos: Vec<String> = Vec::new();
    let mut i = 0;
    while i < args.len() {
        if flags.contains(&args[i].as_str()) {
            flag_vals.push(args.get(i + 1).cloned().unwrap_or_default());
            i += 2;
            continue;
        }
        pos.push(args[i].clone());
        i += 1;
    }
    (flag_vals.first().cloned().unwrap_or_default(), pos.first().cloned().unwrap_or_default())
}

struct GetOpts {
    dir: String,
    pass: String,
    streams: i64,
    segment_mb: i64,
    subfolder: bool,
}

fn parse_get_flags(args: &[String], settings: &config::Settings) -> (GetOpts, String) {
    let mut o = GetOpts {
        dir: settings.download_dir.clone(),
        pass: String::new(),
        streams: settings.streams as i64,
        segment_mb: settings.segment_mb as i64,
        subfolder: false,
    };
    let mut link = String::new();
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "-o" => {
                o.dir = args.get(i + 1).cloned().unwrap_or_default();
                i += 2;
            }
            "-p" => {
                o.pass = args.get(i + 1).cloned().unwrap_or_default();
                i += 2;
            }
            "-n" => {
                o.streams = args.get(i + 1).and_then(|v| v.parse().ok()).unwrap_or(o.streams);
                i += 2;
            }
            "--seg" => {
                o.segment_mb = args.get(i + 1).and_then(|v| v.parse().ok()).unwrap_or(o.segment_mb);
                i += 2;
            }
            "--subfolder" => {
                o.subfolder = true;
                i += 1;
            }
            other => {
                if link.is_empty() {
                    link = other.to_string();
                }
                i += 1;
            }
        }
    }
    (o, link)
}

fn parse_up_flags(args: &[String], settings: &config::Settings) -> (Vec<String>, String, String, i64) {
    let mut paths = Vec::new();
    let mut account = settings.last_account_id.clone();
    let mut folder = settings.last_folder_id.clone();
    let mut streams = settings.streams as i64;
    let mut i = 0;
    while i < args.len() {
        match args[i].as_str() {
            "--account" => {
                account = args.get(i + 1).cloned().unwrap_or_default();
                i += 2;
            }
            "--folder" => {
                folder = args.get(i + 1).cloned().unwrap_or_default();
                i += 2;
            }
            "-n" => {
                streams = args.get(i + 1).and_then(|v| v.parse().ok()).unwrap_or(streams);
                i += 2;
            }
            other => {
                paths.push(other.to_string());
                i += 1;
            }
        }
    }
    (paths, account, folder, streams)
}

/// Prints progress until the job finishes; returns an exit code.
async fn follow(m: Arc<xfer::Manager>, id: &str) -> i32 {
    let tty = is_tty();
    let mut tick = tokio::time::interval(Duration::from_millis(500));
    let mut last_line = Instant::now() - Duration::from_secs(10);
    let mut interrupted = false;
    loop {
        tick.tick().await;
        if !interrupted && signalled() {
            interrupted = true;
            m.cancel(id);
            eprintln!("\nCanceling… (run the same command again to resume)");
        }
        let snap = m.snapshot();
        let Some(v) = snap.jobs.iter().find(|j| j.id == id) else { continue };
        let mut line = format!(
            "{:<9} {:5.1}%  {} / {}  {}/s  files {}/{}  streams {}",
            v.status.as_str().to_uppercase(),
            pct(v.done, v.total),
            human_bytes(v.done),
            human_bytes(v.total),
            human_bytes(v.speed as i64),
            v.files_done,
            v.files_total,
            snap.active_streams
        );
        if !v.phase.is_empty() {
            line = format!("{}  {}", v.status, v.phase);
        }
        if v.eta > 0.0 {
            line.push_str(&format!("  ETA {:.0}s", v.eta));
        }
        if tty {
            print!("\r\x1b[K{}", line);
            let _ = std::io::stdout().flush();
        } else if last_line.elapsed() > Duration::from_secs(5) {
            println!("{}", line);
            last_line = Instant::now();
        }
        if matches!(v.status, Status::Done | Status::Partial | Status::Failed | Status::Canceled) {
            println!(
                "\n{}: {}/{} files, {} in {:.1}s (avg {}/s)",
                v.status.as_str().to_uppercase(),
                v.files_done,
                v.files_total,
                human_bytes(v.done),
                v.elapsed,
                human_bytes(v.avg_speed as i64)
            );
            if !v.error.is_empty() {
                println!("Error: {}", v.error);
            }
            for f in m.files(id) {
                if f.status == Status::Failed {
                    println!("  failed: {} — {}", f.rel_path, f.error);
                }
            }
            return if v.status == Status::Done { 0 } else { 1 };
        }
    }
}

fn pct(a: i64, b: i64) -> f64 {
    if b <= 0 {
        0.0
    } else {
        100.0 * a as f64 / b as f64
    }
}

/// fd 1, as `libc::STDOUT_FILENO`: the progress line is only rewritten in
/// place on a terminal.
const STDOUT_FILENO: i32 = 1;

fn is_tty() -> bool {
    fiotransfer_sys::platform::is_tty(STDOUT_FILENO)
}

fn signalled() -> bool {
    fiotransfer_sys::platform::sigint_ignored()
}

fn fmt_ms(ms: i64) -> String {
    let secs = ms / 1000;
    let days = secs / 86_400;
    let rem = secs % 86_400;
    let (y, m, d) = civil_from_days(days);
    format!("{:04}-{:02}-{:02} {:02}:{:02}", y, m, d, rem / 3600, rem % 3600 / 60)
}

fn civil_from_days(z: i64) -> (i64, u32, u32) {
    let z = z + 719_468;
    let era = if z >= 0 { z } else { z - 146_096 } / 146_097;
    let doe = (z - era * 146_097) as u64;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146_096) / 365;
    let y = yoe as i64 + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let m = (if mp < 10 { mp + 3 } else { mp - 9 }) as u32;
    (if m <= 2 { y + 1 } else { y }, m, d)
}


#[cfg(test)]
mod tests {
    use super::*;
    use proptest::prelude::*;

    // ---------- the reference models ----------
    //
    // The three parsers walk `args` with an index. Each model below is written
    // from the usage text, not from the code: a flag takes the token after it as
    // its value, a switch takes nothing, the first token that is not a value or a
    // switch is the positional (the link, or an upload path), and anything the
    // command does not know is a positional too.

    /// Models `flags_and_positional`: `(first flag value, first positional)`.
    fn model_flags_and_positional(args: &[String], flags: &[&str]) -> (String, String) {
        let mut flag: Option<String> = None;
        let mut pos: Option<String> = None;
        let mut i = 0;
        while i < args.len() {
            if flags.iter().any(|f| *f == args[i]) {
                if flag.is_none() {
                    flag = Some(args.get(i + 1).cloned().unwrap_or_default());
                }
                i += 2;
            } else {
                if pos.is_none() {
                    pos = Some(args[i].clone());
                }
                i += 1;
            }
        }
        (flag.unwrap_or_default(), pos.unwrap_or_default())
    }

    struct GetModel {
        dir: String,
        pass: String,
        streams: i64,
        segment_mb: i64,
        subfolder: bool,
    }

    fn model_get(args: &[String], d: &config::Settings) -> (GetModel, String) {
        let mut m = GetModel {
            dir: d.download_dir.clone(),
            pass: String::new(),
            streams: d.streams as i64,
            segment_mb: d.segment_mb as i64,
            subfolder: false,
        };
        let mut link = String::new();
        let mut i = 0;
        while i < args.len() {
            match args[i].as_str() {
                "-o" => {
                    m.dir = args.get(i + 1).cloned().unwrap_or_default();
                    i += 2;
                }
                "-p" => {
                    m.pass = args.get(i + 1).cloned().unwrap_or_default();
                    i += 2;
                }
                "-n" => {
                    if let Some(v) = args.get(i + 1).and_then(|v| v.parse::<i64>().ok()) {
                        m.streams = v;
                    }
                    i += 2;
                }
                "--seg" => {
                    if let Some(v) = args.get(i + 1).and_then(|v| v.parse::<i64>().ok()) {
                        m.segment_mb = v;
                    }
                    i += 2;
                }
                "--subfolder" => {
                    m.subfolder = true;
                    i += 1;
                }
                other => {
                    if link.is_empty() {
                        link = other.to_string();
                    }
                    i += 1;
                }
            }
        }
        (m, link)
    }

    fn model_up(args: &[String], d: &config::Settings) -> (Vec<String>, String, String, i64) {
        let mut paths: Vec<String> = Vec::new();
        let mut account = d.last_account_id.clone();
        let mut folder = d.last_folder_id.clone();
        let mut streams = d.streams as i64;
        let mut i = 0;
        while i < args.len() {
            match args[i].as_str() {
                "--account" => {
                    account = args.get(i + 1).cloned().unwrap_or_default();
                    i += 2;
                }
                "--folder" => {
                    folder = args.get(i + 1).cloned().unwrap_or_default();
                    i += 2;
                }
                "-n" => {
                    if let Some(v) = args.get(i + 1).and_then(|v| v.parse::<i64>().ok()) {
                        streams = v;
                    }
                    i += 2;
                }
                other => {
                    paths.push(other.to_string());
                    i += 1;
                }
            }
        }
        (paths, account, folder, streams)
    }

    // ---------- generators ----------
    //
    // A token pool with every meaningful class: the flags of both commands, a
    // switch, values that parse, values that do not, an empty token, a token
    // that looks like a flag but is not, a path with a space, and a huge number.

    const TOKENS: &[&str] = &[
        "-o", "-p", "-n", "--seg", "--subfolder", "--account", "--folder", "-x", "--", "link",
        "frame.io/abc", "0", "1", "7", "8", "64", "128", "129", "512", "513", "512.5", "-1",
        "+12", "00012", "999999999999999999999", "", " ", "/tmp/a b", "-o=x", "dir",
    ];

    fn tokens() -> impl Strategy<Value = Vec<String>> {
        prop::collection::vec(prop::sample::select(TOKENS).prop_map(|s: &str| s.to_string()), 0..=9)
    }

    fn settings() -> config::Settings {
        config::Settings::default()
    }

    // ---------- properties ----------

    proptest! {
        #![proptest_config(ProptestConfig { cases: 64, ..ProptestConfig::default() })]

        /// `flags_and_positional` finds the first value of the flag and the first
        /// token that is not a flag or a value, in any order, for any tokens.
        #[test]
        fn flags_and_positional_matches_the_usage_text(args in tokens(), which in 0usize..3) {
            let flags: &[&str] = [&["-p"][..], &["-n"][..], &["-p", "-n"][..]][which];
            let got = flags_and_positional(&args, flags);
            let want = model_flags_and_positional(&args, flags);
            prop_assert_eq!(got, want, "args {:?}", args);
        }

        /// `parse_get_flags` matches the usage text: `-o dir  -p pass  -n N
        /// --seg MB  --subfolder  <link>`, in any order.
        #[test]
        fn parse_get_flags_matches_the_usage_text(args in tokens()) {
            let d = settings();
            let (o, link) = parse_get_flags(&args, &d);
            let (m, wlink) = model_get(&args, &d);
            prop_assert_eq!(link.clone(), wlink.clone(), "link for args {:?}", args);
            prop_assert_eq!(o.dir.clone(), m.dir.clone(), "dir for args {:?}", args);
            prop_assert_eq!(o.pass.clone(), m.pass.clone(), "pass for args {:?}", args);
            prop_assert_eq!(o.streams, m.streams, "streams for args {:?}", args);
            prop_assert_eq!(o.segment_mb, m.segment_mb, "seg for args {:?}", args);
            prop_assert_eq!(o.subfolder, m.subfolder, "subfolder for args {:?}", args);
        }

        /// `parse_up_flags` matches the usage text: `up <path>... --account ID
        /// --folder ID -n N`, in any order; every unknown token is a path.
        #[test]
        fn parse_up_flags_matches_the_usage_text(args in tokens()) {
            let d = settings();
            let (paths, account, folder, streams) = parse_up_flags(&args, &d);
            let (wp, wa, wf, ws) = model_up(&args, &d);
            prop_assert_eq!(paths.clone(), wp.clone(), "paths for args {:?}", args);
            prop_assert_eq!(account.clone(), wa, "account for args {:?}", args);
            prop_assert_eq!(folder.clone(), wf, "folder for args {:?}", args);
            prop_assert_eq!(streams, ws, "streams for args {:?}", args);
        }

        /// Order does not matter: the same flag/value pairs, given in any order,
        /// with the link placed between whole pairs, parse the same way.
        #[test]
        fn get_flags_do_not_depend_on_order(
            keys in prop::collection::vec(any::<u8>(), 0..=4),
            vals in prop::collection::vec("[ -~]{0,8}", 0..=4),
            link_at in 0usize..5,
        ) {
            const FLAGS: [&str; 5] = ["-o", "-p", "-n", "--seg", "--subfolder"];
            let n = keys.len().min(vals.len()).min(5);
            let mut order: Vec<usize> = (0..n).collect();
            // distinct flags only, so "the first one wins" never decides.
            let mut used = std::collections::BTreeSet::new();
            order.retain(|i| used.insert(*i));
            order.sort_by_key(|i| keys[*i]);
            let mut args: Vec<String> = Vec::new();
            for (k, i) in order.iter().enumerate() {
                if k == link_at.min(n) {
                    args.push("https://f.io/abc".to_string());
                }
                args.push(FLAGS[*i].to_string());
                if FLAGS[*i] != "--subfolder" {
                    args.push(vals[*i].clone());
                }
            }
            if link_at >= n {
                args.push("https://f.io/abc".to_string());
            }
            let d = settings();
            let (o, link) = parse_get_flags(&args, &d);
            // the same pairs in the order they were generated
            let mut canonical: Vec<String> = Vec::new();
            for i in 0..n {
                canonical.push(FLAGS[i].to_string());
                if FLAGS[i] != "--subfolder" {
                    canonical.push(vals[i].clone());
                }
            }
            canonical.push("https://f.io/abc".to_string());
            let (c, clink) = parse_get_flags(&canonical, &d);
            prop_assert_eq!(link.clone(), clink.clone(), "link for {:?} vs {:?}", args, canonical);
            prop_assert_eq!(o.dir.clone(), c.dir.clone(), "dir for {:?} vs {:?}", args, canonical);
            prop_assert_eq!(o.pass.clone(), c.pass.clone(), "pass for {:?} vs {:?}", args, canonical);
            prop_assert_eq!(o.streams, c.streams, "streams for {:?} vs {:?}", args, canonical);
            prop_assert_eq!(o.segment_mb, c.segment_mb, "seg for {:?} vs {:?}", args, canonical);
            prop_assert_eq!(o.subfolder, c.subfolder, "subfolder for {:?} vs {:?}", args, canonical);
        }

        /// `-n` is accepted over 1..=128 and `--seg` over 8..=512, with the
        /// documented message outside that, and the boundary values are inside.
        #[test]
        fn ranges_are_inclusive_and_their_message_is_the_documented_one(
            which in 0usize..2,
            off in -3i64..=3,
        ) {
            let (flag, lo, hi, at) = if which == 0 { ("-n", 1, 128, 1) } else { ("--seg", 8, 512, 8) };
            let edge = if off >= 0 { hi + off } else { at + off };
            let inside = (lo..=hi).contains(&edge);
            let got = range_check(flag, edge, lo, hi);
            if inside {
                prop_assert!(got.is_ok(), "{} {} was rejected: {:?}", flag, edge, got);
            } else {
                let want = format!("{} must be between {} and {}", flag, lo, hi);
                prop_assert_eq!(got, Err(want), "{} {}", flag, edge);
            }
        }

        /// A flag at the end of the line has no value: it must not borrow the
        /// link, and it must not panic. `fio get LINK -o` leaves the destination
        /// empty, which `Manager::start_download` rejects with "destination
        /// folder not found"; `-n` with no number keeps the configured value.
        #[test]
        fn a_flag_without_a_value_takes_no_token_from_the_line(prefix in tokens()) {
            let d = settings();
            // A flag at the end of the line has nothing to take: the token before
            // it is a bare word, so the flag is reached unconsumed. It must not
            // borrow the link (or a path), and it must not panic.
            //
            // What it does to its own field is the current behaviour and is
            // reported as an ambiguity: a string flag clears the field, a number
            // flag keeps the value it already had, because a missing number does
            // not parse. An empty destination is then refused downstream
            // ("destination folder not found"), so `fio get LINK -o` still fails.
            for flag in ["-o", "-p", "-n", "--seg"] {
                let mut args = prefix.clone();
                args.push("link".to_string());
                let (base, base_link) = parse_get_flags(&args, &d);
                args.push(flag.to_string());
                let (o, link) = parse_get_flags(&args, &d);
                prop_assert_eq!(link.clone(), base_link.clone(), "{:?}: the flag became the link", args);
                match flag {
                    "-o" => prop_assert_eq!(o.dir.clone(), String::new(), "{:?}", args),
                    "-p" => prop_assert_eq!(o.pass.clone(), String::new(), "{:?}", args),
                    "-n" => prop_assert_eq!(o.streams, base.streams, "{:?}", args),
                    _ => prop_assert_eq!(o.segment_mb, base.segment_mb, "{:?}", args),
                }
            }
            // A switch takes nothing and is idempotent.
            let mut args = prefix.clone();
            args.push("link".to_string());
            let (base, base_link) = parse_get_flags(&args, &d);
            args.push("--subfolder".to_string());
            let (o, link) = parse_get_flags(&args, &d);
            prop_assert_eq!(link.clone(), base_link.clone(), "{:?}: --subfolder ate the link", args);
            prop_assert!(o.subfolder, "{:?}: --subfolder is not a switch", args);
            prop_assert_eq!(o.dir.clone(), base.dir.clone(), "{:?}", args);

            for flag in ["--account", "--folder", "-n"] {
                let mut args = prefix.clone();
                args.push("a.mov".to_string());
                let (base_paths, base_acct, base_folder, base_streams) = parse_up_flags(&args, &d);
                args.push(flag.to_string());
                let (paths, acct, folder, streams) = parse_up_flags(&args, &d);
                prop_assert_eq!(paths.clone(), base_paths.clone(), "{:?}: the flag became a path", args);
                match flag {
                    "--account" => prop_assert_eq!(acct.clone(), String::new(), "{:?}", args),
                    "--folder" => prop_assert_eq!(folder.clone(), String::new(), "{:?}", args),
                    _ => prop_assert_eq!(streams, base_streams, "{:?}", args),
                }
                let _ = (base_acct, base_folder);
            }
        }
    }

    /// The exact strings the command prints for the two numeric flags.
    #[test]
    fn out_of_range_values_are_rejected_with_the_documented_message() {
        assert_eq!(range_check("-n", 1, 1, 128), Ok(()));
        assert_eq!(range_check("-n", 128, 1, 128), Ok(()));
        assert_eq!(range_check("-n", 0, 1, 128), Err("-n must be between 1 and 128".to_string()));
        assert_eq!(range_check("-n", 129, 1, 128), Err("-n must be between 1 and 128".to_string()));
        assert_eq!(range_check("--seg", 8, 8, 512), Ok(()));
        assert_eq!(range_check("--seg", 512, 8, 512), Ok(()));
        assert_eq!(range_check("--seg", 7, 8, 512), Err("--seg must be between 8 and 512".to_string()));
        assert_eq!(range_check("--seg", 513, 8, 512), Err("--seg must be between 8 and 512".to_string()));
    }

    /// The link is the first token that is neither a flag nor a flag's value, and
    /// `--subfolder` is a switch that takes nothing.
    #[test]
    fn the_link_is_the_first_bare_token() {
        let d = settings();
        let a = |v: &[&str]| v.iter().map(|s| s.to_string()).collect::<Vec<String>>();
        assert_eq!(parse_get_flags(&a(&["https://f.io/x"]), &d).1, "https://f.io/x");
        assert_eq!(parse_get_flags(&a(&["--subfolder", "https://f.io/x"]), &d).1, "https://f.io/x");
        assert_eq!(parse_get_flags(&a(&["-p", "pw", "https://f.io/x"]), &d).1, "https://f.io/x");
        assert_eq!(parse_get_flags(&a(&["https://f.io/x", "-n", "4"]), &d).0.streams, 4);
        assert_eq!(parse_get_flags(&a(&["-n", "4", "https://f.io/x"]), &d).1, "https://f.io/x");
        // `-n` with a value that is not a number keeps the configured streams.
        assert_eq!(parse_get_flags(&a(&["-n", "lots", "https://f.io/x"]), &d).0.streams, d.streams as i64);
        // A second bare token is ignored: `fio get` takes one link.
        let (o, link) = parse_get_flags(&a(&["one", "two"]), &d);
        assert_eq!(link, "one");
        assert_eq!(o.dir, d.download_dir);
        // `fio up` keeps every bare token as a path.
        let (paths, _, _, _) = parse_up_flags(&a(&["a.mov", "--account", "x", "b c.mov"]), &d);
        assert_eq!(paths, vec!["a.mov".to_string(), "b c.mov".to_string()]);
    }
}
