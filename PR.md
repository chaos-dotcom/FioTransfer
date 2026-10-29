# The Rust app, property-tested and made sovereign

One branch. It contains the property-testing machinery built for the Rust port, every
defect that machinery found, and the fixes with their regression tests.

Two things this branch does *not* touch: the frontend assets and the window chrome. The
app still looks and behaves as it did for the user.

## Defects found and fixed

Each has a regression test that fails before the fix.

| # | Where | What was wrong | Effect | Test |
| --- | --- | --- | --- | --- |
| 1 | `share::dedupe_remote` | stripped *every* repeated extension instead of one | `a.mov.mov` deduped to `a (2).mov`; a copy could be renamed onto another file's stem | `dedupe_remote_keeps_a_repeated_extension_in_the_stem` |
| 2 | `folderlog::dir_of` / `base_name_path` | empty and trailing-slash paths did not follow the documented rules (`""` → `/`, `a/b/` → `a`) | a trailing-slash upload would write the folder log one level too high | `go_parity_helpers_match_go` table |
| 3 | `xfer::Job::set_files` | seeded per-status counters by incrementing instead of recounting | a file whose status moved before publishing was counted twice: `files_done > files_total` | `publishing_the_file_list_counts_each_file_once` |
| 4 | `share::parse_rfc3339` | accepted impossible dates and a missing offset | `0000-00-00T00:00:00Z` made a working review link look expired | property `rfc3339_…` + `short_timestamps_are_refused` |
| 5 | `api::route_key` | kept the host for a path-less URL; a `://` inside a query was taken for a scheme | distinct routes shared one rate-limit bucket and missed their documented limits | two reproducers, previously `#[ignore]`d |
| 6 | `xfer::Limiter::acquire` | enrolled for a wake-up only after re-reading the count | a release landing in that window was lost; a waiter could sleep until the next release | `prop_sync` (6 properties, 4/4 mutations caught) |
| 7 | `xfer::scan_paths` | followed directory symlinks with no cycle guard | a self-referential link recursed until the stack overflowed | `scan_paths_does_not_follow_a_directory_symlink_cycle` |
| 8 | `api::Client::send` | `Method::from_bytes(..).unwrap()` | a non-token method panicked instead of erroring | covered by the client's error path |

## Property machinery

Model-based suites, each with an independent reference model and a mutation-calibration
table (deliberate defects must be caught, survivors must be fixed by strengthening the
generator):

| suite | area | properties | mutations |
| --- | --- | --- | --- |
| `tests/proptests.rs` | seed suite: limiter, buffer pool, ledger, route keys, paths, CSV | 14 | – |
| `tests/prop_sync.rs` | `Limiter` | 6 | 4/4 caught |
| `tests/prop_state.rs` | upload ledger, download block resume, history store | *(pending)* | |
| `tests/prop_parse.rs` | `route_key`, `safe_name` | 9 | 3/3 caught |
| `tests/prop_watch.rs` | watch folders, folder logs | 7 (+23 lib) | 14/14 caught |
| `tests/prop_shares.rs` | six link providers | 14 (+27 lib) | 30/30 caught |
| `tests/prop_model.rs` | job/file counters, `view()`, `snapshot()`, scanning | 12 (+33 lib) | 24/24 caught |
| `tests/prop_engine.rs` | media-atom parser, part tiling, backoff | 16 | 5 caught, 1 equivalent, 1 survivor→strengthened |
| `tests/prop_bridge.rs` | bridge surface, CLI flags | *(pending)* | |

## Evidence

* `cargo test --offline` → 110 lib tests, 78 integration property tests, 0 failures.
* UI: the app's own webview snapshot, four views at 1180×780 — **0 differing pixels**.
* Performance (same rig as the port's benchmarks): upload 2 GiB / 8 streams 11,018 MB/s,
  8 GiB / 16 streams 10,477 MB/s — at parity with the pre-change numbers.

## Open decisions

* dotfile dedupe (`.hidden` → `.hidden (2)`, staying hidden)
* `skip_name` scope (only the generated `FioTransfer …` log stamp)
* media-atom tolerances (partial trailing header, zero extended size)
