# WO-1b — the Atlas half of token mint / rotation / registration

The **boat daemon half is built and green** (this branch: `internal/token`,
`TunnelHandler` takes a getter, SIGHUP reload, `ExecReload` in the unit). A token
file that is a bare string is the WO-0 static token and never expires; the JSON
form `{"token": …, "hard_expires_at": …}` engages the hard expiry, fail-closed.
Rotation reaches a running daemon by replacing `/etc/boat/token` and
`systemctl reload boat` (→ SIGHUP → `token.Store.Reload`).

What remains is Atlas-side, and is held back deliberately: it changes production
credential flow, needs a schema migration, wants live proof (install a secret →
reload → Atlas reaches the host on the new token), and sits on the SAME
`doctype/server/server.py` install path as the active `feat/boat-http-only`
branch — so it should land in coordination with that work, not blind.

## 1. Mint + store (Atlas Server row)

`atlas/atlas/doctype/server/server.json` — add to the `section_break_boat_mirror`
block:
- `boat_token` — **Password** fieldtype (encrypted at rest, read with
  `self.get_password("boat_token")`), `read_only`, `no_copy`.
- `boat_token_expires_at` — **Datetime**, `read_only`.

`server.py`:
```python
def mint_boat_token(self) -> str:
    """Mint this host's bearer token and its hard expiry (spec/33 §12). Called
    when there is none, or when the current one is within the re-mint window of
    its hard expiry. Short-lived: the daemon serves the last valid one until the
    hard expiry, and Atlas re-mints well before it (BOAT_TOKEN_TTL /
    BOAT_TOKEN_REMINT_BEFORE)."""
    token = secrets.token_urlsafe(32)
    self.db_set("boat_token", token)  # Password field → stored encrypted
    self.db_set("boat_token_expires_at", frappe.utils.now_datetime() + BOAT_TOKEN_TTL)
    return token
```
Pick a TTL and a re-mint window (e.g. TTL 30d, re-mint at 7d remaining) — both
are policy, name them as constants. Every `upgrade_boat`/`bootstrap` and the
mirror sweep is a natural re-mint trigger: if `expires_at` is within the window,
mint before installing.

## 2. Read through the chokepoint

`boat_client.py token_for_server()` — read the Server row first, keep the
site-config fallback so nothing breaks on a host not yet migrated:
```python
row = frappe.get_doc("Server", server_name)
token = row.get_password("boat_token", raise_exception=False)
if token:
    return token
# fall back to the static config (atlas_boat_tokens / atlas_boat_token)
```
Never log it, never put it in an error message (the current docstring already
promises this).

## 3. Install the token file on the host

The token is a per-host SECRET computed at runtime, so it does NOT ride
`_boat_uploads()` (that ships the four static distribution artifacts). Add a
dedicated install step in `_install_boat` (and thus `upgrade_boat`), AFTER the
binary+units and BEFORE `_start_boat`:

```python
def _install_boat_token(self, connection, key_path) -> None:
    token = self.get_password("boat_token") or self.mint_boat_token()
    payload = json.dumps({"token": token,
                          "hard_expires_at": self.boat_token_expires_at.isoformat()})
    # via STDIN, never argv: the secret must not appear in a process list, a
    # command string, or _boat_ssh's error text (§12). run_ssh needs an
    # `input=`/stdin parameter — check its signature; add one if absent. The
    # command reads /dev/stdin, so no `payload` substring is ever in argv.
    run_ssh(connection, key_path,
            "sudo install -m 0640 -o root -g boat /dev/stdin /etc/boat/token",
            input=payload, timeout_seconds=30)
```
Two cautions:
- **0640 root:boat** — the daemon runs as `boat` and must read it; nobody else
  should. `/etc/boat` must exist (bootstrap makes it, or `install -D`).
- **`_boat_ssh` logs stderr/stdout on failure.** The install command must not
  echo the token; if `run_ssh` logs the command string itself, pass the secret
  only on stdin and keep it out of the command.

On the **upgrade** path, after replacing the file, `systemctl reload boat` (not
restart) so the running daemon re-reads it without dropping the tunnel. On the
**bootstrap** path `_start_boat` already `restart`s, which reads it fresh.

## 4. Registration handshake (the deepest piece — live-only)

Today the daemon's address and token both come from site config (spec/33 §4:
"the daemon's address and token still come from site config rather than from a
host that registered itself"). The handshake is:
- `boat bootstrap` (or `_start_boat`) has the daemon learn its own mesh address
  and POST a registration to Atlas (server name + address), and Atlas records
  `mesh_address` + mints+installs the first token.
- OR Atlas drives it entirely over SSH at bootstrap (simpler, no inbound Atlas
  endpoint): mint → install token → record mesh_address from the known droplet
  facts. This is the smaller step and composes with §3 above.
This half genuinely needs the tunnel/mesh stood up (this env has none) and a live
2-host run, so it is the last to land and the one to do WITH a host.

## Tests
`test_server` / `test_boat_client`: mint is idempotent-until-expiry; expired →
re-mint; `token_for_server` prefers the row and falls back to config; the install
step is invoked in `_install_boat`. Use the sys.path-prepend runner against
`atlas.tests.local` (see the atlas-apps memory). The install-to-host + reload +
Atlas-reaches-host-on-new-token path is LIVE proof, deferred.
