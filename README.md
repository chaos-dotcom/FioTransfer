# FioTransfer

A fast Mac app that uploads to Frame.io and downloads from Frame.io links, and logs every transfer the way Frame.io Transfer did.

## Install and run

Double-click `FioTransfer.app` in this folder.

```sh
make app     # rebuild ./FioTransfer.app (signed, ready to double-click)
make cli     # command-line version → bin/fio
make test    # engine tests with fault injection
```

## Downloads

Downloads need no sign-in and no setup.

- **Links it accepts:** `f.io/…`, `next.frame.io/share/…` (V4) and `app.frame.io/reviews/…` (legacy). Links can come from any account.
- **Destination:** you pick the folder, and the link's folder structure is kept.
- **Passphrases:** if a link has one, the app asks for it and remembers it in your Keychain.
- **Download protection:** links where the sender turned downloads off are refused.
- **Resuming:** quitting, crashing or losing the network mid-download is fine. Run the same link to the same folder again and it picks up where it stopped. Finished files are skipped.

## Uploads

Uploads need a one-time connection to Adobe, about 3 minutes.

1. In the [Adobe Developer Console](https://developer.adobe.com/console), choose **Create project**.
2. Choose **Add API → Frame.io API → User Authentication → OAuth Single Page App**.
3. Set both **Default redirect URI** and **Redirect URI pattern** to `https://localhost:8765/callback`.
4. Paste the **Client ID** into **FioTransfer → Settings** and click **Sign in with Adobe**.
5. The first time, macOS asks for your password to trust FioTransfer's local `localhost` certificate. If you skip it, the browser shows a warning you can click past.

Use the Single Page App credential: it gets refresh tokens, so you stay signed in. A Native App credential has no redirect field and, per Adobe's docs, no refresh tokens.

After that, pick a destination (account → project → folder) and drop files or folders anywhere in the window. Folder trees are recreated on Frame.io. Failed or interrupted uploads can be retried from History, and only the unfinished files are resent.

## Watch folders

**Watch Folders → Add**: pick a local folder and a Frame.io destination.

- New files upload once they've stopped changing for 10 s, so card offloads in progress are left alone.
- Subfolders are recreated, and existing Frame.io folders are reused.
- Each file uploads once. This is tracked across restarts, and failures retry up to 3 times.
- Files already in the folder are skipped unless you tick **Include existing files**.
- Each day's batches are appended to `FioTransfer Watch YYYY-MM-DD.txt` in the watched folder.
- Watching runs while the app is open.

## Menu bar and login

- Closing the window keeps FioTransfer running in the menu bar (⇅ icon), with no Dock icon. Transfers and watch folders keep going.
- The menu shows live speeds and has Open, Open at Login and Quit. Quit with ⌘Q or from the menu.
- With **Open at Login** (also in Settings) it starts hidden in the menu bar. This uses `~/Library/LaunchAgents/com.fiotransfer.login.plist`, which points at this copy of the app; turn the option off and on again if you move the app.

## Speed

- **Parallel streams:** every transfer is split into parallel HTTP/1.1 streams, 16 by default and adjustable from 1 to 64 in Settings. All transfers share one pool.
- **Downloads:** chunks are written straight into place on disk.
- **Uploads:** parts go directly to Frame.io's S3 storage.
- **Measured speed:** on this Mac's 707 Mbit/s connection, the public Frame.io press kit (231 MB) downloaded at 89–96 MB/s, which is line rate. A single stream managed 37 MB/s.
- **Sleep:** the Mac is kept awake while transfers run.

## Logs

| What | Where |
|---|---|
| History tab (search, retry, per-file details, CSV export) | `~/Library/Application Support/FioTransfer/history.db` |
| Plain-text log, one file per month | `~/Library/Logs/FioTransfer/transfers-YYYY-MM.log` |

## Command line

```
fio inspect <link>                    what's in a link
fio get <link> -o /Volumes/RAID/In    download (resumable; -p passphrase, -n streams)
fio login | whoami | ls | up …        uploads
fio history
```

## Layout

| Path | What it does |
|---|---|
| `internal/xfer` | Engine: stream limiter, segmented resumable downloader, multipart uploader, jobs and history |
| `internal/share` | Link resolver: V4 share GraphQL (anonymous share auth) and legacy v2 review links |
| `internal/api` | Frame.io V4 REST client (rate-limited) |
| `internal/auth` | Adobe IMS OAuth PKCE with a loopback redirect; tokens are stored in the Keychain |
| `internal/history` | SQLite history and the monthly text log |
| `app.go`, `main.go`, `frontend/dist` | Wails desktop app with a plain HTML/CSS/JS UI |
| `cmd/fio` | Command-line front end |
