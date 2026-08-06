# Decision: should boat run on the Atlas controller?

**Status: a decision for the maintainer, not a task.** This lays out the options
and recommends one; nothing is built.

## The situation

A few verbs belong to the *controller*, not a host: `issue-cert` (ACME runs where
the PEMs land), `tunnel-up`/`tunnel-down`, and `mgmt-firewall-apply/confirm/revert`.
Atlas runs them today through `atlas/atlas/local_task.py run_local_task` — a local
subprocess of the repo's `scripts/*.py`, recording a Task row like any host op,
with secrets passed via `env` (never argv).

Boat has already **ported** these verbs (`internal/cert.IssueCert`,
`internal/mgmtfirewall.*`, the tunnel verbs; all reachable as `boat issue-cert`
etc.). But Atlas cannot route them through boat, because boat runs only on hosts
— there is no boat on the controller — so the Python scripts stay and the ported
Go verbs are unreached on this path.

## Options

**A. A boat daemon on the controller.** Install `boat.service` on the Atlas box
(socket-only, no tunnel/token — it is local), and route the controller verbs
through its socket like host verbs.
- For: one transport for every verb; the controller Python is deleted; boat's
  allow-list/least-privilege applies to controller ops too.
- Against: the daemon runs as `User=boat` and would need sudo grants for
  certbot/`wg`/`nft` **on the controller** — a new privileged surface on the most
  sensitive box in the system. These ops are controller *infra* (ACME, the mgmt
  WireGuard, the mgmt firewall), not VM lifecycle — arguably not boat's domain at
  all (spec/33 §13 already puts vendor/DNS/ACME outside boat). A daemon exists for
  a *continuous* reconcile loop; these are one-shots. One more daemon to upgrade
  and keep in step, on the box that can least afford surprises.

**B. Keep `run_local_task` (status quo).** The three verb families stay Python on
the controller.
- For: no new privileged surface on the controller; `run_local_task` already
  records Task rows and keeps secrets out of argv.
- Against: two implementations of each verb (Python on the controller, Go for
  hosts) to keep in sync; the ported boat verbs are dead on this path.

**C. `boat` as a plain CLI on the controller, via `run_local_task`.** Ship the
`boat` binary to the controller and have `run_local_task` invoke
`boat issue-cert …` instead of `python3 scripts/issue-cert.py …` — same transport
(local subprocess), same Task row, same `env`-secret handling, one implementation.
- For: deletes the controller Python (single boat implementation, the WO-6 win);
  **no controller daemon** and **no new sudoers surface** — the CLI runs as
  exactly the user `run_local_task` runs the `.py` as today; keeps the existing
  Task-row + env-secret machinery unchanged. The busybox binary already answers
  these verbs as one-shots.
- Against: an install step puts `boat` on the controller's PATH; no journal /
  quiesce for these ops (correct — they are one-shots, not a reconcile loop).

## Recommendation — C

Route the controller verbs through the **boat CLI under `run_local_task`**, not a
controller daemon. It captures the only real prize here — deleting the duplicate
controller Python so there is one implementation of each verb — without standing
up a privileged daemon on the controller for work that is one-shot
controller-infrastructure rather than the continuous VM lifecycle a daemon is
for. spec/33 §13 already draws vendor/ACME/DNS outside boat's remit; a controller
daemon blurs that line, while a CLI invocation does not.

Concretely: add `boat` to the controller's install (it is already built for the
fleet), teach `run_local_task` (or its callers) to prefer `boat <verb>` where a
ported verb exists, delete the superseded `scripts/*.py` once each is proven, and
leave `run_local_task` itself — the Task row, the `env` secrets, the
`--kebab-case` contract — exactly as it is.

If a future controller op genuinely needs a *reconcile loop* (nothing here does),
reopen A for that op alone rather than moving all three under a daemon now.
