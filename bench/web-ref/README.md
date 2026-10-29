# web-ref - screenshot `frontend/dist` with the egui harness's fixture

A measurement tool, not product code. It draws the web front end
(`frontend/dist`) with the same fixture the egui harness draws
(`egui-ui/tests/snapshots.rs`, `fixture_state()`), at the same size, and saves
one PNG per screen so the two front ends can be compared by pixels.

## Run

```
cd /Users/chaos/work/FioTransfer-rs/bench/web-ref
./render.sh
```

Writes `/tmp/web-shots/{transfers,watch,history,settings}.png` (1180x780,
deviceScaleFactor 1) and prints the checks, the DOM values it read back, and the
four file sizes. Exit code 0 means every check passed.

Flags: `--out DIR`, `--dist DIR`, `--chromium PATH`, `--settle MS`,
`--no-expand`, `--no-toasts`, `--no-events`, `--keep-profile`.

## Depends on

* `node` >= 22 - only the built-ins (`node:http`, `node:zlib`, `fetch`,
  `WebSocket`). **No npm packages, no install step.** Chromium is driven over
  the DevTools protocol directly.
* Chromium at `/Applications/Chromium.app/Contents/MacOS/Chromium` (Chromium
  140 tested); override with `--chromium`.
* `frontend/dist` as built: `index.html`, `app.js`, `style.css`, `fonts/`.
  Nothing in `dist` is modified or copied.

## What it does

1. Serves `frontend/dist` on `127.0.0.1:<random port>`, and rewrites
   `index.html` on the way out to put
   `<script src="/__bridge.js"></script>` in front of
   `<script src="app.js"></script>`. If that tag ever moves, the render fails
   loudly instead of silently drawing a bridge-less page.
2. Launches Chromium headless with `--remote-debugging-port=0` and a throwaway
   profile, attaches to its page target, sets the viewport to 1180x780 at
   deviceScaleFactor 1, and pins locale `en-GB` so the dates read 24-hour
   ("Today, 09:41", not "09:41 AM").
3. Navigates, waits for the fixture's first job row, waits for
   `document.fonts.ready` (Inter is a webfont), then opens the fixture's
   expanded row, adds the fixture's two toasts, and settles.
4. For each of the four views: clicks `[data-view="<view>"]`, waits for
   `#view-<view>.active`, settles, and saves `Page.captureScreenshot` output.
5. Reads the DOM back and compares the values with the fixture, then decodes
   the PNGs (`node:zlib`, unfiltered in JS) and checks 1180x780, non-empty,
   drawn content, and that the four differ pairwise.

## The bridge (`bridge.js`, served as `/__bridge.js`)

Defines, before `app.js` runs:

* `window.go.main.App[method](...) -> Promise` for every method the front end
  can call. Boot asks for `GetState`, `Snapshot`, `UpdateStatus`, `Watches` and
  `TakeServiceRequests`, and the History view asks for `History`; the rest of
  app.js's call sites are answered with the empty/default shape. The object
  is a `Proxy`, so a method that is not answered rejects loudly and is recorded
  in `window.__bridge.unhandled` (the render prints it and fails).
* `window.runtime.EventsOn(name, cb)` (+ `EventsOff`, `EventEmit`),
  `ClipboardSetText`, `ClipboardGetText`, `BrowserOpenURL`.
  `window.__bridge.emit(name, payload)` fires the handlers; the render uses it
  to emit `snapshot` and `state-changed` once with the fixture payload.

## The fixture

`fixture_state()` in `egui-ui/tests/snapshots.rs`, translated into the JSON the
web front end reads. Field names and values are the egui ones:

| egui | JSON |
| --- | --- |
| `logged_in`, `user{name,email}` | same |
| `settings{client_id, redirect_port, streams, segment_mb, download_dir, no_folder_logs, no_update_check, last_folder_label}` | same object |
| `snapshot.jobs[up-1,dn-1]` (id, kind, title, source, dest, status, phase, total, done, speed, eta, files_*, started_at, finished_at, elapsed, avg_speed, link) | same rows |
| `snapshot.speed, active_streams, stream_limit` | same |
| `files` (2 x `FileView`) | answered by `JobFiles('up-1')` |
| `history` (3 rows: title, kind, status, when, files, size) | `History(...)` rows, with `when` turned back into `created_at` |
| `watches` (2 rows: local, label, direction, enabled, pending) | `Watches(...)` rows |
| `toasts` (ok + err) | drawn through the page's own `toast()` |
| `expanded: Some("up-1")` | the row is opened by clicking its `.a-files` button |
| `destination` | `settings.last_folder_label` |

### Values that had to be invented

The egui model has no equivalent for these; each is also marked `INVENTED` in
`bridge.js`.

* `version: "1.0.0"` - `#about` and `#app-version` print a version, `UiState`
  carries none.
* `redirect_uri: "http://127.0.0.1:8765/callback"` - built from
  `settings.redirect_port`; the settings screen prints it.
* `log_file` / `log_dir` - `/Users/editor/Library/Logs/FioTransfer/fio.log`;
  the fixture has no log path.
* `home: "/Users/editor"` - `shortPath()` needs it to shorten the paths the
  fixture uses (the web row then reads `~/Downloads/FioTransfer`).
* `open_at_login: false` - a checkbox state the egui fixture does not have.
* `settings.last_folder_id` / `last_account_id` - the web destination chip only
  shows `last_folder_label` when an id is set; the egui model keeps the label
  alone. `last_folder_path` is deliberately absent so the chip falls back to
  the label's own ` / ` split and reads exactly like the fixture's string.
* download job `started_at` = `finished_at - elapsed` (754 s).
* history: `id`, `created_at`/`started_at`/`finished_at` (rebuilt from the
  fixture's words "Today, 09:41", "Yesterday, 18:02", "27 Oct, 11:15"),
  `avg_bps` (28.5 MB/s for the finished download, 0 elsewhere), `peak_bps: 0`,
  `done_bytes`, `source`, `dest`, `link`, `error`.
* watches: `name` (basename of `local_path`), `state` (`watching`/`paused` from
  `enabled`), `uploaded` (holds the fixture's `pending`), `downloaded: 0`,
  `failed`, `canceled`, `last_transfer: 0`, `link`, `acct`, `folder`,
  `change`.
* `UpdateStatus` - a whole object: `{available:false, state:"idle",
  current:"1.0.0", latest:"", url:"", blocker:"", progress:0, checked_at:0,
  error:""}`. The egui fixture has no update state, and `checked_at: 0` keeps
  the "Up to date" line empty.

### What could not be faked honestly

* **Toasts.** The web front end has no fixture/state channel for them; they only
  exist if something calls `toast()`. `app.js` is a classic script, so its
  top-level `toast` is on `window`; the render calls
  `window.toast('Share link copied', 'ok', 600000)` and the error one. If
  `app.js` ever becomes a module or a bundle, that path closes and the web shot
  loses the toast block (`--no-toasts` shows that state on purpose).
* **The expanded row.** No state field either; the render clicks `.a-files` on
  `up-1` and answers `JobFiles` with the fixture's two `FileView`s.
* **Wails-only host surface.** `window.runtime` is only used for `EventsOn` and
  `ClipboardSetText`; the rest (`--wails-draggable`, `--wails-drop-target`, the
  native traffic lights) is CSS and window-chrome decoration that neither front
  end draws, so the 40 px `.traffic` strip stays empty ink in both.
* **Web-only columns with no egui source**: History's speed and duration
  columns, the per-watch counters, the watch `name`, the redirect URI, the log
  path, open-at-login and the app version. They are drawn from the invented
  values above and will read as extra ink in a pixel diff.

## Known non-issues

* `favicon.ico` is answered with 204 so the page console stays clean.
* `setInterval(loadWatches, 2000)` keeps calling `Watches`; the answer is the
  same fixture, so the screenshots are stable. Two runs produced byte-identical
  PNGs.
