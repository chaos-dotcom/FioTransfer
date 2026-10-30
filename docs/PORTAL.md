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
| **Access token** | The `PORTAL_API_TOKEN` from Portal's `.env` (generate with `openssl rand -base64 32 \| tr '+/' '-_' \| tr -d '='`). Saved to the login keychain; never written to `settings.json`, never displayed again. Optional: see "Signing in instead" below. |
| **Sign in instead** | Your Portal admin email and password. FioTransfer calls Portal's own login, keeps the session cookie, and throws the password away. Use this when the deployment has no `PORTAL_API_TOKEN`. |
| **Local folder** | Where the project folders live. Default `~/Live_Work`. |
| **Enabled** | Off by default: nothing is contacted until you turn it on. |

### Signing in instead of a token

`PORTAL_API_TOKEN` is optional in Portal and `deploy.sh` never writes one. While it is unset,
every `/api` endpoint accepts only a browser session cookie, so no bearer token can work however
correct it is. On such a deployment, sign in:

1. **Server URL**: your Portal's address.
2. **Sign in instead**: the admin email and password you use in Portal's own login screen, then
   **Sign in**.
3. The row then reads **Signed in as <email>** and a **Sign out** button replaces the fields.

What is kept: the session cookie Portal sets (`portal_session`, fourteen days), in the login
keychain under its own item, and your email in `settings.json` so the field is prefilled next
time. What is not kept, anywhere: the password. It exists for the length of that one request, is
never written to `settings.json`, never logged, and never shown by the app again.

Precedence: if a bearer token is stored, it wins — that is what a headless install has. Otherwise
the session is used. A session Portal stops accepting is reported as *"the Portal session
expired"*, which is a different message from *"Portal refused the token"*: the fix is to sign in
again, not to look for a token. Portal accounts with a second factor (TOTP or a passkey) cannot
complete this sign-in from the app; use a bearer token there.

Portal marks its session cookie `Secure` even when it is reached over plain http
(`SESSION_COOKIE_SECURE` defaults to true). Over `https://` the cookie is sent exactly as Portal
wrote it. Over plain `http://` to another machine it is sent too, without the `Secure` attribute:
the password already crossed that same connection. Use `https://` for any Portal that is not on
this machine.

### If the token is refused

`PORTAL_API_TOKEN` is **optional in Portal itself**: `deploy.sh` writes a `.env` without it, and
while it is unset every `/api` endpoint accepts only a browser session cookie — a bearer token
cannot work, however correct it is. Even then Portal answers `401 Authentication required`, which
looks identical to a wrong token.

So if the status line says *"Portal refused the token"*, either **sign in** with the admin email
and password (see above), or make sure the deployment has a token:

```sh
ssh you@your-portal-host
cd /path/to/portal            # where deploy.sh and .env live
grep -c '^PORTAL_API_TOKEN=' .env || echo 'PORTAL_API_TOKEN=…' >> .env
./deploy.sh --update          # pulls, recreates; data and transfers survive
```

Generate the value with `openssl rand -base64 32 | tr '+/' '-_' | tr -d '='`. That same string goes
into FioTransfer's **Access token** field. There is no admin screen that mints one: the bearer token
is the single value in Portal's `.env`, on purpose.

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

# the sign-in path, against a Portal that holds no token
PORTAL_LIVE_URL=http://127.0.0.1:18080 PORTAL_LIVE_EMAIL=… PORTAL_LIVE_PASSWORD=… \
  cargo test --offline -p fiotransfer --test portal_live -- --nocapture
```

That test serves three links (one good marker, one `../etc`, one `.`) and asserts one
mirrored watch, two stated refusals, and an idempotent second run; with the sign-in
variables it also asserts the sign-in itself, a refused password that stores nothing, and
a session whose cookies are gone reading as expired rather than as a refused token. Each
case is skipped unless its variables are set, so ordinary test runs stay offline. The
password is read from the environment only: it is never a constant in the test file.

## What it does not do yet

* Portal's sync rules are read for reference only; FioTransfer does not adopt them.
* Nothing is sent back to Portal — a link's state in Portal is unaffected by what
  FioTransfer does with its folder.
* The token cannot be rotated from the app; change it in Portal and re-enter it here.
* A second-factor (TOTP or passkey) sign-in is not something the app can complete: sign in
  with a browser and use a bearer token on such a deployment.
