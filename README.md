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

## Status: walking skeleton

This is WO-0. Today Boat can:

- **start** a provisioned VM through its `firecracker-vm@<uuid>.service` unit,
  resuming from a memory snapshot when one is staged;
- **stop** a VM, cooperatively by default so the guest syncs before the unit goes
  down;
- **observe** one VM or all of them — status read off the host, plus host facts
  and the running version;
- remember both in a bbolt file, so re-posting an `operation_id` returns the
  first result instead of running the work twice.

That is the entire surface. Not yet here: adoption and Firecracker re-attach,
whole-host export and the watch stream, fencing, the reconciler, per-VM
networking, the wake-on-TCP reflex, migration, bootstrap, self-update, and the
other ~50 host verbs. Each is its own work order, and until its verb ports the
Python script on the host remains the implementation. Do not deploy this
expecting a control plane.

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
internal/api/         the HTTP surface, implementing the generated interface
internal/model/       the records Boat persists
internal/paths/       every on-host path for a VM, derived from its UUID
internal/run/         the only package that runs a subprocess
internal/store/       bbolt: observed VMs and the operation journal
internal/version/     this build's identity, stamped at link time
internal/vm/          start, stop, observe
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
says why it is there, and the sudoers file names the one command it could not pin
as tightly as the rest.

## Spec

`spec/33-boat.md` in the [Atlas repo](https://github.com/frappe/atlas) is the
source of truth for this repo's contract — the operation set, the desired/observed
split, and the correctness invariants. `llm/Taste.md` here is how the code that
implements it is written.
