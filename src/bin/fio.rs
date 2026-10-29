
//! `fio` — the command-line front end to the FioTransfer engine.
//!
//!   fio inspect <link> [-p passphrase]
//!   fio get <link> [-o dir] [-p passphrase] [-n streams] [--subfolder]
//!   fio login | logout | whoami
//!   fio ls [account-id [folder-id]]
//!   fio up <path>... --account <id> --folder <id>
//!   fio history [-n 20]

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
    if v < lo || v > hi {
        fatal(&format!("{} must be between {} and {}", flag, lo, hi));
    }
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

fn is_tty() -> bool {
    unsafe { libc::isatty(libc::STDOUT_FILENO) == 1 }
}

fn signalled() -> bool {
    unsafe { libc::signal(libc::SIGINT, libc::SIG_DFL) == libc::SIG_IGN }
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
