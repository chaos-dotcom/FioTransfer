# Property sweep — the Space delta (Space view + Frame.io size fields)

Branch `prop/space`, off `origin/main` `82fb79a` ("Merge pull request #7 from
chaos-dotcom/space-view"). Worktree `/tmp/prop-space`, target dir
`/tmp/target-space`, `cargo ... --offline` throughout.

The method is `docs/proptest-praxis.md`. This sweep covers only what landed
after the port's first sweep: the Space view (`167e955` webview, `3a43207` egui)
and the storage walk behind it (`6e8bd2f`).

## What the target actually is at this revision

| The brief asked for | At `origin/main` 82fb79a |
| --- | --- |
| `api::Account.storage_limit`, `.storage_usage` | **Not present.** `src/api.rs:583` `struct Account { id, display_name }` |
| `api::Project.storage` | **Not present.** `src/api.rs:599` `struct Project { id, name, root_folder_id, workspace_id }` |
| the `limit - usage` aggregation | no such subtraction anywhere in the tree |
| "walked size only when the reported size is absent" | no reported size to prefer; the walk is the only source |
| the Space view's aggregation and confirm machine | present: `src/app.rs` (`SpaceProject`, `refresh_space`, `order_space`) and `egui-ui/src/views/space.rs` |

Evidence for the two missing rows: `grep -rn 'storage_limit\|storage_usage'`
over `src/`, `egui-ui/`, `frontend/`, `legacy-go/` finds nothing, and the
uncommitted `core/` extraction in `/Users/chaos/work/FioTransfer-rs` does not
contain them either. The quota change had not landed when this sweep ran, so the
quota deserialisation and the `limit - usage` property are **not covered** — see
"Could not reach" below.

Paths tested instead, all in the root crate plus the egui crate:

| Concern | Path used |
| --- | --- |
| quota-bearing structs → nearest existing | `src/api.rs` `Account`, `Project` (+ `Workspace`, `Asset`) |
| the walk's aggregation inputs | `src/app.rs` `SpaceProject`, `order_space` |
| the numbers the Space screen shows | `egui-ui/src/views/space.rs` `total_bytes`, `SpaceRow::share`, `slices`, `padded` |
| the confirm state machine | `egui-ui/src/views/space.rs` `show_with_actions` driven by `egui_kittest` |

## Files

* `tests/prop_space.rs` — new, root crate (10 tests).
* `egui-ui/tests/prop_space.rs` — new, egui crate (8 tests).
* `egui-ui/Cargo.toml` — `[dev-dependencies] proptest = "1.12.0"` (already in
  `Cargo.lock` as the app's own dev-dependency; resolves offline).
* `src/app.rs` — one extraction, behaviour-identical: the walk's inline
  `out.sort_by(|a, b| b.bytes.cmp(&a.bytes))` became `order_space(&mut out)`, and
  `pub fn order_space(rows: &mut [SpaceProject])` holds the same one-liner with
  the contract in its doc comment. Without a nameable function the ordering
  contract could only be tested by re-typing the comparator in the test, which
  proves nothing.

## Exact run commands

```
cd /tmp/prop-space
export CARGO_TARGET_DIR=/tmp/target-space

cargo test --offline -p fiotransfer      --test prop_space   # 10 tests, 0.02 s
cargo test --offline -p fiotransfer-egui --test prop_space   #  8 tests, 0.82 s
```

Mutations were applied to `src/app.rs` / `egui-ui/src/views/space.rs`, the two
suites re-run, and the file restored byte-for-byte (`diff` clean) after each.

## Properties, cases, oracles

Root crate (`tests/prop_space.rs`):

| Id | Property | Cases | Oracle |
| --- | --- | --- | --- |
| P1 | absent fields default to `Default`, unknown keys ignored, no leak into a known field | 48 | stated contract; the unknown keys are always present |
| P2 | a field that is a string is read whatever its neighbours hold (hostile sibling values, unknown keys) | 48 | stated contract; no panic |
| P3 | no JSON shape panics the parser: a document, a list, or the `{"data": [...]}` page, for `Account`/`Project`/`Asset`/`Workspace` | 48 | `catch_unwind` around every parse |
| P4 | `Asset.head_version`: absent and `null` are `None`; a present head gives its size; a head of the wrong type is an error, not an invented head | 48 | stated `Option<Box<Asset>>` contract |
| P5 | `order_space`: sizes never increase down the list; no row lost, duplicated or invented | 64 | non-increasing scan + multiset of ids |
| P6 | ties keep the order the walk found them in | 64 | **independent insertion-sort model** (`reference_order`), plus a direct first-seen-first-kept check |
| P7 | ordering is idempotent (a second pass is a no-op) | 64 | self-comparison |
| P8 | `SpaceProject`'s serialised field names and count are exactly what the egui `SpaceRow` reads | 64 | the consumer's key list, restated |
| F1 | *finding, pinned*: `null` on any field rejects the whole document | — | current behaviour, asserted |
| F1b | *finding, pinned*: `refresh_space` swallows that error | — | source assertion on `src/app.rs` |

Sizes are drawn from a small domain on purpose (`0..=4` weighted highest) so
ties — the thing "stable" is about — are the common case, alongside `0`,
`i64::MIN`, `i64::MAX` and arbitrary `i64`.

egui crate (`egui-ui/tests/prop_space.rs`):

| Id | Property | Cases | Oracle |
| --- | --- | --- | --- |
| P1 | `total_bytes` is the sum of the non-negative sizes: never negative, zero exactly when nothing is held, order-invariant | 48 | `i128` reference sum |
| P2 | `share(total)` is a finite fraction in [0, 1]; 0 for a row that holds nothing or a total of 0; the exact ratio otherwise; all-positive rows divide 1.0 between them | 48 | f64 ratio recomputed from the definition |
| P3 | `slices` partition the circle in row order: contiguous starts, no negative span, no wrap past 360°, only the last slice stretched and it closes at 360°, and a zero-byte row still advances the colour ramp | 48 | partition arithmetic recomputed from the row list |
| P4 | `padded(span)`: the gap is capped at 0.9° and never takes more than half the slice | 48 | the two constants restated |
| P5 | the confirm machine: opening reports nothing; Cancel / close cross / Escape / Enter / backdrop click report nothing and close the dialog; only "Delete on Frame.io" reports, exactly one `Delete`, naming the pressed row — and several more frames report nothing more | 36 (× 6 dismissal paths, 1–4 rows, shared or unique names) | the state machine driven end-to-end through kittest (`egui_kittest::Harness`), reading only `show_with_actions`'s reported actions |
| P6 | two rows with the same name but different sizes are two different confirmations: the pressed one is named | 36 | project id of the pressed row |
| P7 | one confirm click is one delete, however many frames follow | 36 | action count |
| F2 | *finding, pinned*: `total_bytes` overflows past `i64::MAX` | — | current behaviour, asserted |

P5's generators cross the interaction classes deliberately: row count × pressed
row × unique/shared project name × six dismissal paths. 36 cases × 6 paths means
every (count, name-mode, path) class is reached many times.

## Mutation calibration

Applied one at a time, both suites re-run, then reverted.

| # | Mutation | File | Result | Caught by |
| --- | --- | --- | --- | --- |
| M1 | `total_bytes`: `row.bytes.max(0)` → `row.bytes as u64 as i64` — a negative size becomes enormous, the unsigned-subtraction family applied at the clamp | `egui-ui/src/views/space.rs` | **caught** | egui P1, P2, P3 (3 of 8) |
| M2 | `order_space`: `b.bytes.cmp(&a.bytes)` → `a.bytes.cmp(&b.bytes)` — biggest-first flipped to smallest-first | `src/app.rs` | **caught** | root P5, P6 |
| M3 | `confirm_dialog`: the cancel branch returns `Decision::Delete` instead of `Decision::Cancel` — confirm fires on cancel | `egui-ui/src/views/space.rs` | **caught** | egui P5 |
| M4 (bonus) | `confirm_dialog`: drop `modal.should_close()` from the cancel condition — Escape and the backdrop click stop closing the dialog | `egui-ui/src/views/space.rs` | **caught** | egui P5 (the "left the dialog open" assertion) |

No mutation survived. M4 exists because M3 alone would not prove the Escape and
backdrop cases in P5 reach `ModalResponse::should_close` rather than passing for
an unrelated reason; M4 shows they do. After the reverts both suites are green
again (10 and 8 tests).

## Findings

### F1 — `null` on any API field fails the whole page, and the Space walk swallows it

Every field of `api::Account` / `api::Project` / `api::Workspace` / `api::Asset`
is a plain `String` / `i64` with `#[serde(default)]`. `serde(default)` covers an
**absent** key; it does not cover a key that is present and `null`, so serde
rejects the whole document.

Minimal reproduction (in `tests/prop_space.rs`, `f1_a_null_field_rejects_the_whole_document`):

```rust
let account: Result<Account, _> =
    serde_json::from_str(r#"{"id": "acct-1", "display_name": null}"#);
assert!(account.is_err());   // "invalid type: null, expected a string"
```

Why it is worth reporting rather than accepting:

1. The Go original accepts that document. `encoding/json` unmarshals `null` into
   a `string`/`int64` as a no-op and reports no error
   (`legacy-go/internal/api/api.go:200-228`), so this is a port regression on
   exactly the inputs the original tolerated.
2. The failure is one level up as well as one row: a single `null` anywhere in a
   page fails the whole `Vec<Account>` / `Vec<Project>` / `Vec<Asset>`.
3. **The consequence is silent.** `App::refresh_space` (`src/app.rs`) has

   ```rust
   let accounts = match self.api.accounts(&cx).await {
       Ok(a) => a,
       Err(e) => { self.space_walking.store(false, Ordering::SeqCst); let _ = e; return; }
   };
   ```

   so a rejected accounts page leaves the Space tab on its empty block —
   "Nothing counted yet" — with no error text, indistinguishable from an account
   holding nothing. `walk_folder` errors, by contrast, do reach the row as
   `error` ("could not finish").

Confidence: the parse behaviour is demonstrated; that Frame.io ever sends
`null` for these fields is **not** demonstrated. The visible half of the finding
(an error the walk discards) holds whatever the cause of the `Err`. Severity:
low if the API never sends `null`, moderate if it does, because the symptom is a
blank screen.

Minimal fix if wanted: `#[serde(default, deserialize_with = "null_as_default")]`
on the scalar fields, or `Option<String>`/`Option<i64>` with accessors that
flatten `None`. Not applied here — this branch is the discovery suite.

### F2 — the total overflows past `i64::MAX`

`total_bytes` sums `i64`s unchecked: `rows.iter().map(|row| row.bytes.max(0)).sum()`.
Two rows of `i64::MAX` panic in a debug build and wrap negative in a release
one, where the page head prints a negative size and the donut's share of the
whole inverts.

```rust
let rows = vec![row(0, i64::MAX), row(1, 1)];
std::panic::catch_unwind(|| total_bytes(&rows)).is_err()   // debug
```

Not reachable from Frame.io in practice (that is exabytes), so this is a
robustness note, pinned as `f2_the_total_overflows_past_i64`. The fix, if ever
wanted, is `fold(0i64, |a, b| a.saturating_add(b.max(0)))`.

## Could not reach

1. **The quota fields themselves** — `Account.storage_limit`,
   `Account.storage_usage`, `Project.storage` do not exist at this revision (see
   the table). The three properties the brief names for them —
   `left = limit - usage` never negative, `None` when the limit is `None`, and
   the walked size used only when the reported size is absent — have no code to
   run against. They should be re-run when the quota change lands; the harness
   shape (a generator over `(Option<i64> limit, i64 usage)` pairs plus a
   reference model in `i128`) carries over unchanged.
2. **`App::refresh_space` and `App::walk_folder`** — both need a live Frame.io
   account: `AuthManager` plus `reqwest` over the network. So the walk's
   summation across children and folders, the depth cap at 32, the
   `space_walking.swap(true)` re-entry guard, the `(Instant, Vec<SpaceProject>)`
   cache, and `delete_project`'s `retain` after a successful delete are covered
   only indirectly (through the ordering and the payload shape). Covering them
   needs an injectable API trait, which this revision does not have.
3. **The `"Space"` bridge command** (`src/app.rs`, backtick-marshalled JSON) —
   reachable only through the same live `App`. P8 checks the payload's field
   names against the egui consumer's, which is the part that can rot silently.
4. **Font/name edge cases in the confirm dialog** — labels are matched exactly,
   so rows whose names are prefixes of each other, or names containing the
   dialog's own words, are not exercised. Shared names are (P5's `same_name`
   variant and P6).
5. **egui snapshot tests** for the Space screen (`egui-ui/tests/snapshots.rs`)
   were not re-blessed; this sweep adds nothing that draws.
