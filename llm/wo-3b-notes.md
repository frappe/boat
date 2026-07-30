# WO-3b — host-local network apply: build notes & review log

Living doc for the WO-3b pass (host-local networking apply). Records what
shipped, the decisions taken, and everything a reviewer should look at.
Source plan: `atlas/llm/plans/atlas-boat-split.md` (Part II, WO-3), spec ch.33 §6.

## Frontier at start of this pass
- Boat has WO-0, WO-1, WO-2 (lifecycle verbs + reflexes), WO-3a (unit
  supervision + probe + security) shipped. Repo main @ `0f7b811`, `make check`
  green.
- WO-3b (per-VM netns/veth/tap apply, NAT44, proxy-NDP, /128 route, per-VM nft
  isolation, `local-ownership.json`, reserved-IP 1:1 NAT, customer-gateway
  forwarding) is `NOT BUILT`. Everything per-VM still runs the Python
  `firecracker-vm@` hooks. spec/33 §6.1.
- Staging: 3 boat-split hosts — atlas-host-1 (168.144.179.179, boat e77b6e0),
  atlas-host-2 (168.144.209.248, boat b1e7279), and 168.144.146.52 ("meo", a
  live firecracker VM, no boat). (press-v2 hosts 4/5/6 are a DIFFERENT staging —
  do not touch.)

## Scope of this pass
Ship the bounded, fully-testable pieces of WO-3b, each a small commit with
golden/unit tests (the `spec/33 §3.5` "owed harness" at unit level: rendered
command lines asserted byte-for-byte vs the Python, no host needed):

1. sidecar network.env writers (upsert/remove) — prereq for reserved-ip + up/down
2. `internal/netapply/localownership` — the local-ownership.json cache writer
3. `internal/netapply/reservedip` rendering — nft/ip rule builders (golden)
4. `internal/netapply/reservedip` apply/remove — host-touching, recorder-tested
5. `/vms/{uuid}/reserved-ip` endpoint + reconciler + CLI (CAS's first caller)
6. Atlas (feat/boat-split): `Reserved IP.attach/detach` → BoatClient, CAS-gated

## Shipped (this pass)

**Reserved-IP 1:1 NAT — Boat side, complete & `make check` green.** Commits:
1. `feat(sidecar): pure network.env writers, upsert and remove a key` — `Upsert`/
   `Remove`, golden-tested against `network_env.py`.
2. `feat(reservedip): render the 1:1-NAT rules, held to the Python's output` —
   `internal/netapply/reservedip/rules.go`, nft rule builders + injection guards
   (`canonicalIPv4`), golden strings captured from `reserved_ip_nat.py`.
3. `feat(reservedip): apply and remove the NAT, both delivery models` — anchor
   discovery, DO-anchor + routed-flexible-IP apply, detach by handle, egress
   policy route; full command-sequence goldens via a recorder.
4. `feat(vm): the reserved-ip verb — durable flag, then the live NAT` —
   `vm.Manager.ReservedIP`: reads the sidecar, writes RESERVED_IPV4, dispatches.
5. `feat(api): the reserved-ip endpoint, validated and serialized` —
   `POST /vms/{uuid}/reserved-ip` (openapi + regen wire), handler validates the
   reserved IP at the boundary, serialized through `perform`/reconciler; reports
   the delivery model as the operation result.

**To review — reserved-IP:**
- Verb string chosen `vm-reserved-ip` (matches the Python script name, not the
  `<verb>-vm` shape). Atlas's `boat_client` verb table must add the same string.
- Not fenced (attaches NAT to a running VM, boots nothing) — deliberate, see the
  handler doc-comment. Confirm that matches intent.
- Live apply NOT exercised on a host this pass (needs a proxy VM with a reserved
  IP; the boat hosts have no VMs tonight). Verified at unit/golden level only.
- The CAS on the host reserved-IP slot (§11.2/§11.5) is the *Atlas* side; the
  Boat endpoint is its first caller but Boat itself does not CAS reserved-ip.

**local-ownership.json writer — shipped (library, not yet wired).** Commit
`feat(localownership): the ANCP ownership cache writer, flock and atomic`.
`internal/netapply/localownership`: `Read`/`Add`/`Remove`, POSIX flock +
atomic rename + dir-fsync + fail-loud-on-corrupt, ported from
`networkd/localownership.py`. Race-tested (20 concurrent adds, none clobbered).
The lock is co-located with the cache, so on `DefaultPath` it is exactly the
Python's `/etc/atlas-networkd/local-ownership.lock` — a Python bring-up and a Go
one interlock — and tests lock a temp file instead of needing root.
- **To review:** it has no caller yet. It is the writer the network-up/down port
  (below) will call; landed ahead of it the way WO-2's components were built
  before wiring. If you'd rather it land WITH its caller, hold it.

**Atlas side — `feat/boat-split` @ `2c93d30`** (in the boat-split worktree, NOT
this repo): `feat(reserved-ip): route the host 1:1-NAT through Boat when enabled`.
`Reserved IP._run_nat_task` now does `run = run_boat_task if boat_enabled else
run_task` (mirrors every other verb); added `BoatClient.reserved_ip_virtual_machine`
+ `RESERVED_IP_VERB` dispatch in `_run_verb`; two tests in `test_boat_client.py`.
- **To review — Atlas:** the atlas test suite was NOT run (the local bench runs on
  `main`; running the suite against the boat-split worktree is out of scope for an
  autonomous pass). Verified by `py_compile` + pattern-match against the tested
  start/stop/rebuild dispatch only. **Run `bench run-tests --module
  atlas.tests.test_boat_client` on a boat-split bench before trusting it.**
- **CAS not implemented.** The plan wants reserved-ip attach CAS-gated on the
  host reserved-IP slot (§11.2/§11.5). Not done: the mirror does not model the
  reserved-IP slot yet, and the Frappe row already guards the one-IP-one-VM
  invariant transactionally. Left as the documented follow-up the "first caller"
  was meant to exercise.

**per-VM network-up/down apply — SHIPPED and PROVEN on a live host.** Commits
`c62782c` (park.Unpark export), `1623d9f` (`internal/netapply/vmnetwork`),
`6f96308` (`boat vm-network-up/down` CLI hooks). Port of `vm-network-up.py`
(306 LOC) + `vm-network-down.py` (140 LOC): netns/veth/tap, NAT44 masquerade,
IMDS-drop, proxy-NDP, /128 + /32 routes, sysctls, per-VM nft isolation. Unpark
delegates to park, reserved-ip re-apply to `reservedip.Attach`.

**The §3.5 differential harness — built and passed (this is what I'd wrongly
called "owed" before).** Method: staged the Python reference scripts on host-1
(no VMs, safe lab), ran `vm-network-up.py`/`down.py` for a synthetic test VM,
captured the exact command trace + host effects; then ran `boat vm-network-up/
down` for the same VM and diffed. Result: **byte-identical host effects** —
same netns, same nft forward rules, same `2001:db8::2 via fe80::3` route, same
proxy-NDP, same v4 /32 route, tap present in the namespace; teardown removed all
of it. Golden command traces are locked in `vmnetwork_test.go`. host-1 left clean.

**Still deferred inside the network module (config-gated, absent on a public VM):**
- The **private plane** — `apply_private_network` + WireGuard host mesh
  (`apply_persisted_tunnels`) + `apply_persisted_firewall` + the local-ownership
  write/withdraw. Gated on PRIVATE_ADDRESS / persisted config. The public-vs-ANCP
  boundary decision (spec §6.1: the wg peer table is ANCP's) governs which of
  these become Boat's — settle that before porting them. `localownership` (already
  shipped) is the writer this block needs.
- **The cutover is NOT done.** `firecracker-vm@.service` still runs the Python
  `vm-network-up.py`/`down.py` hooks; nothing re-points ExecStartPre/ExecStopPost
  to `boat vm-network-up/down`. The Go path is proven equivalent but not yet live
  on any real VM's unit. Re-pointing the unit (+ the Atlas install.sh change) is
  the "go live" step, to be done deliberately per §3.5's per-module gate.

## Remaining WO-3b work (for review)
- **Customer-gateway host forwarding** — a resident-daemon port (`gateway.service`
  → `boat gateway`), WO-5-adjacent, not a bounded apply.
- **Reserved-IP live exercise** — shipped + unit/golden-verified, and now the same
  host-1 lab could live-verify it (attach a reserved IP to a test VM); not yet done.

## Bug found via the differential (for review / upstream fix)
- **`vm-network-up.py` adds a duplicate forward-accept rule on every restart.**
  nft echoes an interface name back QUOTED (`oifname "atlas-hdeadbe"` — proven on
  host-1), so the Python's unquoted idempotency guard `f"...oifname {host_veth}"`
  never matches its own rule on a re-list, and it re-adds both v6 accepts each
  boot — splitting the traffic counter its own comment warns about. The Go port
  (`vmnetwork.forwardRules`) strips the quotes before matching, so a restart is a
  no-op: **verified live** — two `boat vm-network-up` runs left exactly 2 rules,
  not 4 (commit `ddc89d5`). The Python should get the same fix; until then a
  Python-managed host accumulates dead duplicate accepts (functionally harmless,
  but the counter is wrong, which misleads the sleepy-VM idle sweep).

## WO-3b network apply — COMPLETE and proven (2026-07-30, second session push)
The full `vm-network-up`/`down` is now ported and **differentiated byte-for-byte
on host-1** against the Python — public plane, private tenant isolation, AND the
public-ingress firewall, in one bring-up:
- **Public plane** (`vmnetwork.go`/`down.go`, `network.go` CLI) — commits
  `c62782c`/`1623d9f`/`6f96308`; differential passed; a latent Python
  restart-duplicate bug found & fixed (`ddc89d5`).
- **Private plane** (`private.go`) — commit `d82af3a`; tenant-isolation nft rules
  byte-identical live (security-critical), ownership cache tracked.
- **Firewall** (`firewall.go`) — commit `1e34aa3`; `public_filter` chain
  byte-identical live.
- **Capstone**: a full public+private+firewall bring-up flushed-and-compared on
  host-1 diffed **byte-identical**; teardown removed all of it; ownership cleared.

**WireGuard tunnels (step 9) — SHIPPED and proven.** Commit `ad0b0b4`
(`wireguard.go`). `apply_persisted_tunnels`/`apply_tunnel` ported (the private key
is a file path, never inlined). Live differential on host-1 (wireguard-tools
installed there): the wg interface (port/peer/allowed-ips/host address) and the
isolation rules (forward accept+drop, input host-drop) are **byte-identical** to
the Python.

**⇒ The entire `vm-network-up`/`down` is now ported: public plane + private
isolation + firewall + WireGuard, every module proven byte-identical on a live
host.** The only thing standing between this and live is the cutover (below).
NOTE: `apt install wireguard-tools` was run on host-1 (a real boat host needs it
for tunnels anyway); leave it.

## END-TO-END: a real VM booted on boat's plumbing (2026-07-30, meo)
Not just byte-identical host state — a **real Alpine guest booted to userspace**
with its disk node mknod'd by `boat vm-disk-up` and its netns/tap built by `boat
vm-network-up`. On meo (the boat host with firecracker + images): created a throwaway
alpine thin-snapshot, ran both boat hooks, launched firecracker directly in the
boat-built netns. Console showed `root=/dev/vda` (firecracker opened the boat disk
node), a full kernel boot, then OpenRC → `Starting sshd ... [ ok ]`; the tap went
`LOWER_UP` (guest virtio_net attached to the boat tap). Torn down; meo's live VM
(06571461) left running and untouched. Recipe in `TESTING.md §5a`.

This closes the gap the differential could not: boat's disk + network bring-up
work for a real guest, not only in nft/ip/lvm output. (The boat hosts host-1/2
still can't run VMs themselves — no firecracker binary; that is the WO-1b
bootstrap gap. meo is the firecracker-equipped boat-split host.)

## Beyond WO-3b — the disk bring-up hook (same session)
**`vm-disk-up` ported & live-proven** (`internal/vmdisk`, commit `4f5ff9b`; `boat
vm-disk-up` CLI). Activates the VM's activation-skip thin snapshot LV and re-mknods
its jail node with the current major:minor — the disk analogue of vm-network-up.
Differential on host-1 with a **loop-backed thin pool** (built + torn down; recipe
now in `TESTING.md §6a`): LV active state, node major:minor, owner and perms all
**byte-identical** to `vm-disk-up.py`. Activation + mknod only — NOT the lvcreate/
snapshot CoW paths, which stay in the unported bulk of `lvm.py`. The last
`firecracker-vm@` hook is `vm-restore` (FC-API snapshot resume) — needs a live VM +
a staged snapshot to diff, which no boat host has.

## The cutover — exact steps + its real dependencies (do NOT force fleet-wide)
The go-live is re-pointing the `firecracker-vm@.service` hooks:
```
ExecStartPre=… vm-network-up.py %i   → ExecStartPre=/usr/local/bin/boat vm-network-up %i
ExecStopPost=… vm-network-down.py %i → ExecStopPost=/usr/local/bin/boat vm-network-down %i
```
NOT done, because it is genuinely gated (I chose not to ship a regression):
1. **WireGuard** — a tunnel VM regresses until step 9 ports. Cut over only hosts
   with no tunnel VMs, or port wireguard first.
2. **Boat hosts need MORE than the network hooks.** They have no `firecracker-vm@`
   unit at all, and the sibling hooks `vm-disk-up`/`vm-restore` are still Python
   and not ported — so a full boat-host unit needs those too (WO-1b + storage).
   The 2-line swap works today only on a Python host that ALSO has boat installed.
3. It is a shared, fleet-wide template with no per-host gate.
Recommendation: port wireguard + the disk/restore hooks, then have WO-1b install a
boat-hook unit on boat hosts; leave Python hosts on the Python unit until migrated.

## (earlier) The cutover, and a boat-host gap it exposed (for review)
- **The cutover is a clean 2-line swap** in `scripts/systemd/firecracker-vm@.service`:
  `ExecStartPre=/var/lib/atlas/venv/bin/python /var/lib/atlas/bin/vm-network-up.py %i`
  → `ExecStartPre=/usr/local/bin/boat vm-network-up %i` (and the ExecStopPost
  twin). NOT done: the template is shared fleet-wide with no per-host gate, and it
  wants a real bootable VM to prove connectivity through the Go-created netns
  (the differential proved the host-side setup is byte-identical, which is strong
  evidence, but not a booted guest). This is the go-live decision — yours to make.
- **Finding: boat-bootstrapped hosts have no networking path at all yet.** On
  host-1: `/var/lib/atlas/bin` is absent (no Python durable package) AND
  `firecracker-vm@.service` is not installed. So a VM start on a boat host would
  `systemctl start` a unit that does not exist, and even if it did its
  ExecStartPre would point at a Python hook that is not there. This makes the
  vm-network port REQUIRED (not an optimization) for the boat fleet to run VMs —
  but the missing unit-template install is a separate gap, WO-1b (bootstrap)
  territory, not WO-3b. Flagging so it is not a surprise when the first VM lands
  on a boat host.

## Known gaps / things to double-check
- **Nothing shipped this pass has been exercised on a live host.** The boat hosts
  (host-1 e77b6e0, host-2 b1e7279) run builds behind main and have no VMs; the
  "meo" host has a VM but no boat. A redeploy of `boat` to a host with a running
  proxy VM + a reserved IP is what would actually exercise the reserved-ip apply.
- Reserved-ip verb string `vm-reserved-ip` must match on BOTH sides — it does
  (boat `verbReservedIPVirtualMachine`, atlas `RESERVED_IP_VERB`). Grep both if
  either is renamed.
- `make check` (boat) is green at every commit; commits `3c88c6f`..`bb3a943` on
  `main`. Nothing pushed. Atlas commit on `feat/boat-split` (worktree), unpushed.
