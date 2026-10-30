# Property sweep — the Space delete path (delete on Frame.io only)

Increment: what landed **after** the port sweep. Base `origin/main` at the time of
the run: `9aa4e78` ("space: how much is left — the account's own storage …"),
which merged `82fb79a` ("Space view: what Frame.io is holding, per project, with
deletion"). Branch: `prop/delete` (worktree `/tmp/prop-delete`).

## Paths used

No `core/` exists in `origin/main` (`git ls-tree origin/main` has none), so the
root crate's delete path was tested, not an extraction:

| what | where |
| --- | --- |
| the delete the Space view runs | `src/app.rs::App::delete_project` |
| the HTTP call under it | `src/api.rs::Client::delete_project` (`DELETE /v4/accounts/{acct}/projects/{project}`) |
| the history line | `src/history/mod.rs::Store::log` → `<log_dir>/transfers-YYYY-MM.log` |
| the row it evicts | `src/app.rs::SpacePayload::projects` (`App::space()`) |
| the bridge that reaches it | `src/app.rs`, arm `"DeleteProject"` (a one-liner over `App::delete_project`) |

## Files

* `tests/prop_delete.rs` — the suite (new).
* `src/app.rs` — a **test seam only**: `App::new` now builds through
  `App::assemble`, and `App::with_api(settings, Arc<Client>)` lets a test supply
  a real `Client` whose `base_url` points at a stub. No behaviour change;
  nothing in the crate calls `with_api`.

## Run

```
CARGO_TARGET_DIR=/tmp/target-delete cargo test --offline --test prop_delete
# the three finding reproductions (they fail on purpose):
CARGO_TARGET_DIR=/tmp/target-delete cargo test --offline --test prop_delete -- --ignored
```

Runtime: 4.0–4.4 s for the two green properties, 0.1 s for the probes.

## How the path is driven

`App::delete_project` runs for real. A one-thread stub HTTP server answers the
API: `GET` account/workspace/project/children routes, and `DELETE` either `200`
(mode `Deletes`) or `404` (mode `Refuses`), with generous
`x-ratelimit-limit`/`x-ratelimit-window` headers so the client's real token
bucket never waits. Each case gets its own sandbox:

```
<tmp>/sandbox/                       <- the case's working directory
  media/keep.MOV                     sentinel files, a fake watch-folder layout
  media/sub/deep.bin                 (media/watch/In, media/watch/Out,
  media/sub/sub2/leaf.txt             media/watch/upload.fiopart), a symlink,
  media/watch/...                     and an empty dir
  media/link_to_keep -> keep.MOV
  Library/Logs/FioTransfer/          the case's real history log
  Library/Application Support/...    the case's real SQLite store
```

The Space cache is seeded by the real `refresh_space()` walking the stub's two
accounts (`acct/projA`, `acct/projB`, `acct2/projC`).

The oracle is independent of the app: whether a delete happened is read from the
stub's request log (a 2xx `DELETE`), never from the return value.

## Properties and case counts

| # | property | cases | result |
| --- | --- | --- | --- |
| P1 | `deleting_a_project_never_touches_a_local_file` — for any sequence of 0..8 deletes with Frame.io answering 200: every sentinel file under `media/` is byte-identical (sha256 + length + kind, so an addition, a rename or a removal is an inequality), nothing outside `Library/` changed at all, one log line per accepted delete names the project and contains "nothing local was touched", and the Space cache equals the wire model | 48 | pass |
| P2 | `a_refused_delete_changes_nothing_and_keeps_the_row` — same sequences with Frame.io answering 404: the error reaches the caller, the media tree and everything outside `Library/` are untouched, **no** log line is written, and the row is still in the cache after the failure | 48 | pass |
| P3 | `a_project_id_cannot_change_the_delete_route` (the id space is P1/P2's: `""`, `.`, `..`, `../files/x`, `a/b`, `projA/../projB`, `%2e%2e%2fprojB`, the sandbox's own absolute paths, `media/watch`, spaces, a non-BMP char, random names) | 32 | **fails → F1** |
| P4 | `one_accounts_delete_keeps_the_other_accounts_row` | 1 | **fails → F2** |
| P5 | `one_delete_writes_exactly_one_log_line` | 1 | **fails → F3** |

The generator reaches the interacting cases on purpose: repeated ids, an id that
is not in the cache, an id equal to a cache row in another account, an empty
account and project, and ids that are real paths inside the sandbox.

## Mutation calibration

Three defects planted in `App::delete_project`, one at a time, each reverted with
`git checkout -- src/app.rs`, then the baseline re-run.

| mutation | caught? | what caught it |
| --- | --- | --- |
| M1 `let _ = std::fs::remove_dir_all(project);` before the API call | **caught** | both P1 and P2: `the media tree changed after [...]` (the snapshot differs) |
| M2 the cache row dropped **before** the API call | **caught** | P2 only: `a failed delete changed the Space cache (account "acct", project "projA")` |
| M3 the log line without the project id (`"SPACE  deleted project on Frame.io (nothing local was touched)"`) | **caught** | P1: `log line does not name project "projC": "… deleted project on Frame.io (nothing local was touched)"` |
| none (baseline, after all three reverted) | — | green, `2 passed; 0 failed; 3 ignored` |

The calibration runner lives at `/tmp/prop-delete-mut/calibrate.py`; it builds
with cargo and then runs the test binary with `HOME=<scratch>`, so a mutation
that reached `config::dir()` could not write into the real user profile.

**Incident worth recording.** The first calibration run of M1 destroyed the
worktree it was running in: M1 deletes the path named by the project id, and the
id pool contains `.` and `..`, which resolve against the *test process's* working
directory — the package root, whose parent is `/private/tmp`. Nothing outside
that worktree was lost ( `..` failed once `.` was gone), but the harness now runs
every case with `set_current_dir` inside its own sandbox, behind a process-wide
mutex, with the sandbox nested one level down so `..` reaches only the case's own
temporary directory. Re-run M1 after that change: caught by the snapshot, sandbox
only.

## Findings

### F1 — a project id changes which Frame.io resource the DELETE reaches (live, unencoded id)

`src/api.rs::Client::delete_project` pastes the id into the URL:

```rust
self.call(cx, "DELETE", &format!("/v4/accounts/{}/projects/{}", acct, project), &[], None).await
```

`reqwest`/`url` then parses that string, and the URL standard applies dot-segment
removal *and* treats `%2e` as a dot. Measured on `9aa4e78` (stub request line, one
`DELETE` per row, `acct` = `acct`):

| project id | what went over the wire |
| --- | --- |
| `""` | `DELETE /v4/accounts/acct/projects/` — the collection |
| `"."` | `DELETE /v4/accounts/acct/projects/` — the collection |
| `".."` | `DELETE /v4/accounts/acct/` |
| `"../files/x"` | `DELETE /v4/accounts/acct/files/x` — a **file** route |
| `"../../v4/me"` | `DELETE /v4/accounts/v4/me` |
| `"projA/../projB"` | `DELETE /v4/accounts/acct/projects/projB` — a different project |
| `"a/b"` | `DELETE /v4/accounts/acct/projects/a/b` — an extra path segment |
| `"%2e%2e%2fprojB"` | kept verbatim (`%2e%2e%2f` is not decoded on the way out) |
| `" projA "` | `%20projA` — spaces *are* encoded |
| `"p1\n…"` | newline dropped by the URL parser, rest percent-encoded |

Nothing local is touched — the promise this sweep is about **holds**, and that is
what P1/P2 establish. What fails is the narrower promise inside the same function:
that the id names exactly one project. Today's callers are safe in practice (the
Space view passes ids from its own walk, and `refresh_space` only ever sees
Frame.io's ids); the exposure is that the bridge takes these two arguments from
the webview (`window.go.main.App.DeleteProject(acct, project)`), and any future
caller, script or paste can name another resource.

Reproduction: `cargo test --offline --test prop_delete -- --ignored` (P3), which
shrinks to an id such as `".."`, or the table above.

**Candidate fix, verified green but not committed here.** In `Client::delete_project`, refuse
anything that is not one path segment, and encode what is left. Note that
percent-encoding *alone* does not fix it: `%2E%2E` is a dot segment to the URL
parser, so a guard is required:

```rust
fn seg_ok(s: &str) -> bool {
    !s.is_empty() && !s.contains(['/', '\\', '%']) && !s.trim_start_matches('.').is_empty()
}
if !seg_ok(acct) || !seg_ok(project) {
    return Err(ApiErr::Other(format!("refusing to delete project {:?}: not one path segment", project)));
}
```

With that patch applied on `9aa4e78`, P3 passes and P1/P2 stay green (a refused id
is an error before any request is sent); F2 and F3 are untouched. `app.rs`'s
`delete_project` and the Space view would then have to decide what to show when an
id is refused — an error row, not a delete.

### F2 — cache eviction ignores the account (latent)

`payload.projects.retain(|p| p.project_id != project)` drops **every** row with
that project id, in every account. Two rows for the same id in different accounts
mean deleting one project removes the other account's row from the view.

```
left:  {("acct", "projB")}
right: {("acct", "projB"), ("acct2", "projA")}   projA in acct2 is still on Frame.io
```

Unreachable through the Space view as shipped (Frame.io project ids are UUIDs,
globally unique), so this is a latent coupling, not live data loss. Reproduction:
P4. A minimal fix is to keep the account in the predicate
(`!(p.account_id == acct && p.project_id == project)`), which also needs the caller
to pass the account — it already does.

### F3 — the log line is not sanitised (low)

`h.log(&format!("SPACE    deleted project {} on Frame.io (nothing local was
touched)", project))` writes the id raw. An id containing `\n` writes a **second,
forged** line into the transfer log:

```
2026-10-30 01:52:32  SPACE    deleted project p1
SPACE    deleted project X on Frame.io (nothing local was touched) on Frame.io (nothing local was touched)
```

One delete, two lines; the fake one looks exactly like a real entry. An empty id
also leaves a line that names nothing (`deleted project  on Frame.io`). Fix: escape
the id (or log only ids that passed F1's guard). Reproduction: P5.

## What I could not reach

* **The bridge itself.** `DeleteProject` is a one-line arm over `App::delete_project`
  (read, not executed): reaching it needs the WebKit/egui host. Its argument
  coercion (`s(&args, i)` → `""` for `null`/non-string) is why the empty-id case is
  in the generator.
* **A real Frame.io.** "The project is gone" is the stub's answer, so a server that
  answers 200 without deleting, or 204 with a body, is not modelled. `ApiErr::
  ambiguous` (5xx) retry behaviour is not exercised: 5xx makes `send` retry with
  backoff, which would blow the runtime budget.
* **Concurrency.** Two deletes at once, or a delete during `refresh_space`, are not
  generated. The cache is behind one mutex, so no property was asserted about
  interleaving.
* **A mutation writing into the user profile.** The snapshot covers the case's
  sandbox and "anything outside `Library/`"; the app's own `config::dir()` /
  `config::log_dir()` are the caller's HOME, so the calibration runs the test
  binary with `HOME=<scratch>`. A property that asserted the real profile is
  untouched would have to own that directory.
* **`x-ratelimit` / `page_size` encoding aside.** `Client::url` percent-encodes
  query *names* too, so `page_size` goes out as `page%5Fsize`. Servers decode it, so
  it is benign; it is noted because it is visible in every stub request.
