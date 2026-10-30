# Connecting FioTransfer to Portal

Portal names the projects; FioTransfer does the moving. Files still flow only between
FioTransfer and Frame.io — Portal never carries a byte of media.

## What Portal has to say

Portal marks an upload link's Frame.io folder with a `.turbosort` file whose content is
the `Live_Work` folder name that project belongs to. Its admin API serves the same value
as `turbosort` on each row of `GET /api/upload-links`, alongside the `folder` object
(`account_id`, `folder_id`, `folder_name`) it publishes into.

FioTransfer reads that list and makes sure a **mirrored watch folder** exists for every
link that carries a usable marker:

    ~/Live_Work/<turbosort>   <->   Frame.io / <folder_name>

Two-way: uploads and downloads both follow changes. The local root is yours — FioTransfer
owns the path and creates the folder; Portal's own `destination_path` / `path_template`
are deliberately ignored, because the NAS layout Portal writes for Project Zeus is not
the layout of this machine.

## Setting it up

Settings → **Portal**:

| Field | What goes in it |
| --- | --- |
| **Server URL** | Your Portal, e.g. `https://portal.yourdomain.com`. Must be `http://` or `https://` with a host. |
| **Access token** | The `PORTAL_API_TOKEN` from Portal's `.env` (generate with `openssl rand -base64 32 \| tr '+/' '-_' \| tr -d '='`). Saved to the login keychain; never written to `settings.json`, never displayed again. |
| **Local folder** | Where the project folders live. Default `~/Live_Work`. |
| **Enabled** | Off by default: nothing is contacted until you turn it on. |

Then press **Reconcile now**. The status line tells you what happened:

    2 min ago · 2 links, 3 folders kept in step, 1 removed
    Failed 2 min ago: can't reach Portal: connection refused

Folder counts appear only when the last reconcile succeeded — a failure is never dressed
up as a success. While **Enabled** is on, FioTransfer re-checks every 30 seconds
(`portal_every_secs`); while it is off, nothing goes out.

## What FioTransfer will and will not do

* It only ever touches watches it created. Those carry `origin = portal:<link id>`; a
  watch you made by hand has an empty origin and is never edited or removed.
* A marker must be exactly one path segment. `.`, `..`, `a/b`, `a\b`, `a%2f` and control
  characters are refused with a stated reason, and nothing is created on disk. Portal
  enforces the same rule when the marker is written, so the two sides agree by
  construction.
* When a link disappears, is switched off, or its marker is cleared, the folder it
  created is **unwatched — not deleted**. Removing local media is always your decision.
* Reconciling twice changes nothing the second time.

## Checking it without your Portal

```sh
# a real instance, local, no Frame.io needed for the reconcile itself
PORTAL_LIVE_URL=http://127.0.0.1:18080 PORTAL_LIVE_TOKEN=… \
  cargo test --offline -p fiotransfer --test portal_live -- --nocapture
```

That test serves three links (one good marker, one `../etc`, one `.`) and asserts one
mirrored watch, two stated refusals, and an idempotent second run. It is skipped unless
both variables are set, so ordinary test runs stay offline.

## What it does not do yet

* Portal's sync rules are read for reference only; FioTransfer does not adopt them.
* Nothing is sent back to Portal — a link's state in Portal is unaffected by what
  FioTransfer does with its folder.
* The token cannot be rotated from the app; change it in Portal and re-enter it here.
