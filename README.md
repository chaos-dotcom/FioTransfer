# FioTransfer in Rust

This repository is the Rust application. The original Go app is kept under
`legacy-go/` for reference and for the before/after measurements; it is not
built and not shipped.

A port of [FioTransfer](../FioTransfer) (Go + Wails) to Rust. The window, the
frontend, the menu bar item, the download panel and the Finder services are the
same as the Go app's — the same bytes, the same WebKit engine, the same
Objective-C — and the transfer engine, the history store, the share-link
providers, the updater and the app itself are Rust.

```
make app          # build FioTransfer.app (ad-hoc signed)
make cli          # build bin/fio      (the command-line front end)
make bench        # build bin/fio-bench (the measuring harness)
make test         # cargo test
```

## What is byte-identical, and what is Rust

| Piece | Go app | This port |
| --- | --- | --- |
| `frontend/dist/{index.html,style.css,app.js,fonts/*}` | served by Wails | **the same files, unmodified** (sha256 verified), served to the same WKWebView |
| `Info.plist`, icon, `LICENSE`, `THIRD_PARTY_NOTICES.txt` | Wails template | **the same bytes** |
| `tray_darwin.m`, `quick_darwin.m`, `services_darwin.m` | cgo + Go callbacks | **the same files**, compiled as-is, calling into Rust |
| main menu, panels and alerts | Wails | new Objective-C (`mac/window.m`, `mac/dialogs.m`) doing what Wails did |
| engine, app, history, share providers, updater | Go | Rust |

The frontend talks to `window.go.main.App[method](...)` and
`window.runtime.EventsOn(...)`, so `src/gui.rs` injects a small script that
provides exactly those, backed by IPC. `app.js` is not modified in any way.

## Verification

### The user interface: pixel for pixel

Screen capture is not available to a background session, so both builds
photograph **their own webview** (`takeSnapshotWithConfiguration:`): a build
with `-tags shot` for Go, `FIO_SHOT_FILE` for Rust. Both are driven to the same
view with the same JavaScript, in a 1180×780 window, under the same `HOME`.

`shasum` of the four views, Go versus Rust, 1,180 × 780 = 920,400 pixels each:

| View | Differing pixels |
| --- | --- |
| Transfers | **0** |
| Watch Folders | **0** |
| History | **0** |
| Settings | **0** |

The DOM agrees too: `document.body` is the same HTML (the only difference found
was the download folder in the title attribute, when the two builds ran under
different `HOME`s).

### The numbers

Measured on the same machine (macOS 26, Apple silicon), same isolated `HOME`,
same file set, same settings, both builds ad-hoc signed, both stripped.

| | Go | Rust | |
| --- | --- | --- | --- |
| Executable | 15,649,760 B (14.9 MiB) | **8,647,184 B (8.2 MiB)** | **-44.7%** |
| `.app` bundle | 15 MB | **8.4 MB** | -44% |
| Idle RSS (app process) | 119.5 MB | **103.2 MB** | **-13.6%** |
| Upload, link-limited to 250 MB/s | 249.4 / 251.8 MB/s | 250.1 / 245.4 MB/s | parity |
| Upload, 1 stream, 64 MiB parts | 1,538 MB/s | 1,477 MB/s | -4% |
| Upload, loopback, 4 streams | 8,302 MB/s | **11,481 MB/s** | **+38%** |
| Upload, loopback, 8 streams | 9,995 MB/s | **10,501 MB/s** | +5% |
| Upload, loopback, 16 streams | 9,766 MB/s | 9,667 MB/s | -1% |
| Upload, loopback, 8 GiB, 16 streams | 9,632 MB/s | **11,151 MB/s** | **+16%** |
| Upload, loopback, two endpoints | 8,192 MB/s | **11,281 MB/s** | **+38%** |
| Upload after a 50% interruption | 2.15 GB re-sent | **1.17 GB re-sent** | **-45%** |
| Download, 16 streams, 64 MiB segments | 4,204 MB/s | **5,177 MB/s** | **+23%** |
| Download after a ~50% interruption | 1,879 MB re-fetched | **1,073 MB re-fetched** | **-43%** |
| Unit tests | 4,655 lines of Go tests | 61 tests | |

RSS is the app process after 14 s, three runs each; the WebKit helper is a
separate XPC process with the same footprint on both sides, so it is left out of
both numbers. Sizes are the release builds the Makefile makes.

The upload numbers come from `bin/fio-bench`, which drives each build's real
engine against one sink process (`../bench/sink`, a stand-in for Frame.io's API
and storage) with 8 × 256 MiB files unless the row says otherwise. Read them as
follows:

* **Link-limited (250 MB/s): the two are the same.** Both saturate the link; the
  remaining difference is noise. This is the case that matters for a transfer
  app over the internet.
* **On loopback, Rust is ahead nearly everywhere**: +38% at 4 streams, +16% on
  the 8 GiB run, +38% when parts spread over two endpoints. The exception is the
  shortest run at 16 streams (-1%), where both are pinned against the sink's own
  ceiling and the difference is noise.
* **Go degrades as endpoints are added** (9.8 GB/s on one, 8.2 on two) while
  Rust climbs (11.3 on two). Frame.io hands parts to several storage hosts in
  practice, so this is the row that matters most.
* **An interrupted upload is much cheaper in Rust**: the second attempt sends
  only the missing parts (see *Resumable uploads* below).

The Python stand-in (`../bench/fake_frameio.py`) is slower and noisier than the
sink; its numbers are not quoted above.

### What was not verified

* Sign-in against Adobe IMS (no credentials here): the PKCE flow, the local
  CA/trust step and the Keychain store are implemented, but only the pieces
  that do not need a real account were exercised.
* Anything against the real Frame.io API: every request path was exercised
  against the stand-in servers only.
* The menu bar item, the download panel, the file panels and notifications were
  compiled and unit-harnessed, but never shown on a real screen from this
  session; their Objective-C is the Go app's own code.
* The Go test suite (4,655 lines) was not ported. 53 cheap unit tests were
  (paths, timings, escaping, naming, the CSV shape, resume sidecars).


## Resumable uploads

Parts are what make an upload resumable, so the engine keeps a ledger of them.

While a file is uploading, `~/Library/Application Support/FioTransfer/uploads/`
holds one JSON header per file in flight — the Frame.io file id, the part layout,
the presigned part URLs, the file's size and mtime — plus an append-only `.done`
list naming the parts already on Frame.io. Each finished part appends one line;
that is the whole write cost.

If the upload stops (user cancel, quit, crash), the ledger stays and the copy on
Frame.io is **not** deleted. The next attempt at the same file, to the same
folder, with the same size and mtime, resumes: only the missing parts are sent,
and the log says so (`9/32 parts already on Frame.io`).

* **Freshness.** Presigned URLs expire. A ledger older than
  `FIO_UPLOAD_RESUME_SECS` (default one hour) is not trusted.
* **Dead URLs.** If a resumed part answers 403, the file is started again from
  nothing, once, and the log records why. No user action needed.
* **Changed files.** A different size or mtime means the parts no longer describe
  the file: the ledger is dropped and the stale Frame.io copy deleted.
* **Different destination.** Same: dropped and deleted, so nothing is orphaned.
* **Housekeeping.** `clean_up` deletes unfinished uploads whose owner is gone —
  except when a fresh ledger still needs one.

Measured, interrupting an 8 × 256 MiB upload halfway and running it again
(2.15 GiB total, 8 streams, 8 MiB parts):

| | bytes re-sent | time |
| --- | --- | --- |
| Go (no resume) | 2,147,483,648 | 0.222 s |
| Rust (resume) | **1,174,405,120** | **0.119 s** |

That is 45% fewer bytes and half the time for a single interruption. On a real
link, which is what makes a 12 GB render worth interrupting at all, the saving is
the same fraction: the second attempt sends only what is missing.


## Resumable downloads

Downloads resume at **1 MiB block granularity**, not at segment granularity.

`.fiopart` is the target file's bytes so far; `.fiostate` records, per segment,
whether it is done and how many bytes of it are already in the part file:

```json
{"size":268435456,"seg_size":67108864,"done":[true,false,false,false],
 "filled":[67108864,3145728,0,0]}
```

`filled` is optional: a sidecar written by the Go app (or by an earlier build)
has no such field and is still read, resuming at segment boundaries. The Go app
ignores the extra field, so the two can share sidecars. A sidecar with no
progress at all is written exactly as before.

Why the extra field matters: with 64 MiB segments and 16 streams, an
interruption at 50% leaves 16 segments half-written. Recording only whole
segments throws all of that away — measured, the Go engine re-fetches 92% of a
file it had already half downloaded. Recording bytes per segment re-fetches only
what is missing: total bytes served over the two attempts equals the file's size
exactly.

The engine now writes progress per 1 MiB block (throttled to one state save a
second), so a killed process loses at most a second of transfer and never a
whole segment.

Uploads cannot go finer than a part: an S3 multipart part must be sent in one
request, so the part is the smallest resumable unit there — which is why the
upload ledger records parts and the download ledger records blocks.

## Layout

```
src/api.rs        Frame.io V4 client (same routes, limits and retry rules)
src/auth.rs       Adobe IMS PKCE, localhost https callback
src/localcert.rs  the local CA macOS trusts, name-constrained to localhost
src/config.rs     settings.json under ~/Library/Application Support/FioTransfer
src/history/      SQLite history, watch rows, unfinished uploads, monthly log
src/share/        Frame.io shares and review links, Smash, WeTransfer, Dropbox,
                  Google Drive, Portal
src/xfer/         the transfer engine: manager, upload, download, watch folders
src/update.rs     GitHub releases, checksums, bundle swap, relaunch
src/app.rs        the object the frontend binds to (every app.js call)
src/gui.rs        window, webview, asset server, JS bridge
src/tray.rs etc.  the AppKit side panels
src/bin/fio.rs    the CLI
src/bin/bench.rs  the measuring harness
mac/              the Go app's .m files, plus windows/menus and panels
```
