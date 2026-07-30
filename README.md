# Boat

Boat is the per-host daemon for [Atlas](https://github.com/frappe/atlas). It owns
the mechanics of Firecracker microVMs on one machine — starting them, stopping
them, and reporting what is actually true about them — and it is the authority
for that host's **observed** state.

Atlas stays the control plane. It decides intent: which VMs exist, where they go,
what they are for, who owns them, what they cost. It hands each host a desired
state and reads back fact. Boat never learns the other half: it knows a VM as a
UUID plus resource numbers, and nothing it does depends on knowing more.

The point of the split is not transport speed. Atlas already drives hosts over
SSH perfectly well. The point is **where state and decisions live** — a host that
can answer questions about itself, keep running when the control plane is
unreachable, and be re-adopted rather than rebuilt.

## What belongs in Boat

> Boat may decide any question answerable from a VM's UUID plus host-local facts:
> nft counters, on-disk markers, the host signature, free RAM, what the launcher
> supports. Boat may **not** decide any question whose answer would differ based
> on who owns the VM, what runs inside it, or what it costs.

Boat owns realization and reflex; Atlas owns enrolment and intent. Applied:

| Question | Whose |
|---|---|
| Is this VM running, stopped, or sleeping? | Boat — it reads the unit and the markers |
| Does this VM resume from its memory snapshot or cold-boot? | Boat — the on-disk marker decides, not the request |
| Should this idle VM be allowed to sleep, and after how long? | Atlas — enrolment is policy |
| Which host does this VM live on? | Atlas — placement is a fleet question |
| How much RAM is free on this host? | Boat — a raw number |
| Does "sleeping" count as billable capacity? | Atlas — the moment cost enters, it is not mechanics |

Guest identity crosses the boundary as opaque bytes: Boat writes them into the
rootfs without parsing them, so a service-semantic field can never become
something Boat has an opinion about.

## Status: the VM lifecycle, and not much else

Today Boat serves the nine lifecycle verbs — **start**, **stop**, **pause**,
**resume**, **sleep**, **wake**, **resize**, **rebuild**, **terminate** — and
the two calls that define the relationship with Atlas: `PUT /vms/{uuid}` is
Atlas asserting intent, `GET /export` is Boat asserting fact. `DELETE
/vms/{uuid}` is the PUT's mirror — Atlas taking an assertion back, so this host
stops driving a VM it no longer owns. It touches nothing on the host, and it
keeps the fence epoch: retraction ends an authority and must not hand back
permission to boot. Around them:

- **observed state read off the host** — the unit's state and the on-disk
  markers, never a command's exit code — for one VM, for all of them, or for
  the whole host in a single document;
- **an operation journal**, so re-posting an `operation_id` returns the first
  result instead of running the work twice;
- **crash recovery for that journal** — every claim carries the run of the daemon
  that took it, so a restart tells the work a crash abandoned from the work it is
  still doing, drives those VMs before anything else, and closes records that
  would otherwise read `Running` for the life of the host;
- **one actor per VM**, so a verb and a reconcile pass are one queue and nothing
  drives a machine another thing is mid-boot on;
- **the wake-on-TCP reflex**, resident, deciding with no control plane in the
  loop;
- **a fence epoch** that only Atlas issues, without which this host boots
  nothing it finds on its own disk;
- **supervision of the host's own units** — the thin pool, the network control
  plane, the wake trap, the management firewall — reported in `GET /host` and in
  the export, and startable and restartable by name. The verb set stops there:
  there is no stop, because nothing in Boat wants a sibling unit down and
  stopping the wake trap would strand every sleeping VM on the host with nothing
  watching its counter;
- **a compare-and-set on the desired-state PUT** — `If-Match: <observed-epoch>`
  — for a caller that decided something from the mirror rather than merely
  re-asserting it. The token is the whole-host epoch the export carries and the
  comparison is scoped to the VM the request names, because a whole-host
  comparison would be invalidated by every unrelated observation and would refuse
  every write on a busy host.

Almost every verb's whole request is its `operation_id`. A resize reads its
numbers from the desired state Atlas already asserted rather than being sent
them, and the per-VM uid comes off the host's own `network.env` — a request that
could state a shape the store disagrees with is the shape to refuse in review.
Two verbs carry more: `stop` its two knobs, and `rebuild` the source to lay down
plus the guest identity to write into it.

An operation carries the verb's trace and, for the one verb that has one, its
typed **result** — `sleep` reports whether the guest's RAM was captured, and why
not when it was not, because that is decided on the host and stated nowhere else.
Absent is not false: a verb with nothing to report, or one that failed, carries no
result at all.

Not yet here: provisioning, migration, bootstrap, self-update, per-VM networking
as a Boat subcommand, reserved-IP NAT — which is the first caller the CAS above
is waiting for — and the other ~50 host verbs. Each is its own work order,
and until its verb ports, the Python script on the host remains the
implementation. The CLI is narrower still than the API — `boat vm` starts,
stops, lists and shows, and the other seven verbs are reachable only over HTTP.

## The API is the whole surface

Every capability Boat has is an endpoint in [`api/openapi.yaml`](api/openapi.yaml).
The `boat` CLI and the systemd units are **clients of that same surface** — they
are not a second way into the host with powers the API lacks. That is what makes
the CLI a truthful break-glass tool when Atlas is unreachable, and what lets
Atlas drive a host completely.

The daemon listens on a local unix socket and, once a host is registered, on the
Central-managed management tunnel. It never binds a public interface.

## Layout

```
api/openapi.yaml      the Atlas<->Boat contract; the source of truth for the wire
api/codegen.yaml      how internal/wire is generated from it
cmd/boat/             the multi-call entry point: daemon, vm, host, version
internal/adopt/       reading a host's existing VMs, and quarantining the rest
internal/api/         the HTTP surface, implementing the generated interface
internal/fcattach/    re-attaching to a Firecracker that outlived the daemon
internal/fence/       the boot gate: whether this host may boot a UUID at all
internal/hostfacts/   what this host is, measured rather than remembered
internal/journal/     the write-ahead record of a non-idempotent decision
internal/model/       the records Boat persists
internal/park/        the sleeping VM's reachability and the wake-on-TCP reflex
internal/paths/       every on-host path for a VM, derived from its UUID
internal/reconcile/   one actor per VM, driving observed state toward desired
internal/run/         the only package that runs a subprocess
internal/sidecar/     the KEY=value files the host keeps beside every VM
internal/store/       bbolt: observed VMs, desired state, fences, the journal
internal/units/       the host's own units: which ones, their liveness, start/restart
internal/version/     this build's identity, stamped at link time
internal/vm/          the mechanics: one file per verb, plus observe
internal/watch/       the observed-change stream
internal/wire/        generated from the IDL. never hand-edited
systemd/boat.service  the daemon unit
sudoers.d/boat        the pinned NOPASSWD allow-list for the non-root service user
llm/Taste.md          how code here is written
```

## Build and test

```sh
make build      # static binary at bin/boat (CGO_ENABLED=0, version stamped)
make check      # gofmt check + go vet + go test -race
make test       # tests only
make generate   # regenerate internal/wire from api/openapi.yaml
```

`make check` is the gate. The binary is deliberately static and dependency-free:
it is dropped onto a bare Ubuntu host that has no Go toolchain and no libc we
control.

## On a host

One binary backs every Boat unit on the machine — `boat daemon` here, and
`boat networkd`, `boat pool`, `boat wake-trap` as those land — so a host runs
exactly one version of Boat and one swap moves all of it. The daemon runs as the
non-root `boat` user; the individual privileged commands go through the
enumerated allow-list in [`sudoers.d/boat`](sudoers.d/boat).

```sh
sudo install -m 0755 bin/boat /usr/local/bin/boat
sudo useradd --system --home-dir /var/lib/boat --shell /usr/sbin/nologin boat
sudo install -m 0440 -o root -g root sudoers.d/boat /etc/sudoers.d/boat
sudo visudo -cf /etc/sudoers.d/boat
sudo install -m 0644 systemd/boat.service /etc/systemd/system/boat.service
sudo systemctl daemon-reload && sudo systemctl enable --now boat.service
```

Read the comments in both files before installing them; each non-obvious line
says why it is there, and the few lines that cannot be pinned to a single shape —
a VM's own IPv6, a caller-chosen guest path — name the residual risk they carry
and the code-side check that is their first line of defence.

## Spec

`spec/33-boat.md` in the [Atlas repo](https://github.com/frappe/atlas) is the
source of truth for this repo's contract — the operation set, the desired/observed
split, and the correctness invariants. `llm/Taste.md` here is how the code that
implements it is written.
