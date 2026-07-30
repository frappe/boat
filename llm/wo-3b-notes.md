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

## Deferred / remaining WO-3b work (for review) — deliberately NOT rushed
- **The big one: per-VM network-up/down apply** — port of `vm-network-up.py`
  (306 LOC) + `vm-network-down.py` (140 LOC). netns/veth/tap, NAT44 masquerade,
  host IMDS-drop, proxy-NDP, /128 + /32 routes, sysctls, per-VM nft isolation,
  and it also calls `private_network.py` / `wireguard.py` / `firewall.py` (the
  ANCP-adjacent private plane) + `localownership.add/remove` + the reserved-ip
  apply this pass already ported. **Deliberately not started this pass:** it is
  the `firecracker-vm@` ExecStartPre hook — the most restart-sensitive path — a
  rendering slip here is "a VM off the network", §3.5 mandates the live-host
  differential harness before it cuts over, and I had no proxy/test VM to
  differential-test against tonight. This wants a careful dedicated pass on a
  live host, not an overnight port. Note the public-vs-private (ANCP) boundary
  is a real decision inside this module — which of apply_private_network /
  apply_persisted_tunnels / apply_persisted_firewall are Boat's vs stay
  networkd's must be settled first (spec §6.1 says the wg peer table is ANCP's).
- **Customer-gateway host forwarding** (Atlas-computed, Boat-applied) — smaller,
  self-contained; a good next bounded slice after the network-up decision above.
- **Live-host differential harness** (§3.5) still owed. Reserved-ip + everything
  this pass is unit/golden-verified only.

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
