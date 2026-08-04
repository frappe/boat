# WO-5 — boat networkd on HashiCorp memberlist: design

Rewrite Atlas's ANCP network-control-plane daemon (`scripts/lib/atlas/networkd/`,
26 modules, ~6158 LOC) as `boat networkd`, using `github.com/hashicorp/memberlist`
for the gossip substrate. User directive: reduce duplication vs re-porting the
whole SWIM/gossip stack. Citations are `file:line` in the boat-split worktree.

## 0. HARD CUTOVER (stated explicitly)
memberlist frames its own messages (msgpack + SWIM/push-pull, optional AES) —
**wire-incompatible** with ANCP's JSON envelope on plain UDP:7946. A `boat
networkd` host CANNOT gossip with a Python `atlas-networkd` host in either
direction. No mixed-fleet mode, no on-wire bridge. A cluster converts
**all-at-once** (or per isolated private-network cluster). Acceptable per the
directive (Python ANCP is being removed). The only surviving cross-impl contracts
are the **on-disk file seam** (local-ownership.json, seed files, key files) and the
**northbound status** Atlas ingests (§8.3).

memberlist replaces: transport, gossip fan-out, SWIM probe/failure, push/pull
anti-entropy, the JSON wire envelope, per-source rate-limit. We KEEP the
application layer: wg peer/ownership records, ed25519 signing, conflict resolution,
wg-mesh.conf render, the apply pipeline, the local-ownership seam.

## 1. Module map (26 → keep ~1900 LOC, delete ~2400)
Legend: SUBSUMED (memberlist provides), REIMPL (re-port as Go/delegate), REUSE
(existing boat code), DROP.

| Module (LOC) | Class | Note |
|---|---|---|
| transport.py (125) | SUBSUMED | memberlist owns TCP+UDP transport |
| gossip.py (399) | SUBSUMED+REIMPL | fan-out=TransmitLimitedQueue/GetBroadcasts; `_apply_record`→Delegate.NotifyMsg |
| antientropy.py (501) | SUBSUMED+REIMPL | pull machinery=push/pull; `_apply`→LocalState/MergeRemoteState |
| probe.py (305) | SUBSUMED | SWIM ping/ack/indirect = ProbeInterval/Timeout/IndirectChecks |
| failure.py (229) | SUBSUMED+thin REIMPL | alive→suspect→dead = suspicion+EventDelegate. **Retain `ownership_grace`** route-retention (§3.4) |
| join.py (108) | SUBSUMED+thin REIMPL | cold_join = memberlist.Join + rejoin loop |
| seed.py (195) | REIMPL | load+verify seed.json operator detached sig; trust dir; → Join addrs |
| ratelimit.py (72) | DROP | existed for plaintext public UDP; memberlist owns socket + AES gate |
| peers.py (71) | SUBSUMED | m.Members() + internal sampling |
| wire.py (531) | DROP+thin REIMPL | envelope/framing die; retain record (de)serializers |
| records.py (247) | REIMPL | **crown jewel** — data model + monotonic apply + validate() injection guard |
| signing.py (318) | REIMPL | crypto/ed25519; canonical-JSON body + `_kind` domain sep byte-identical |
| keys.py (213) | REIMPL | wg keypair (shell wg genkey/pubkey) + ed25519 + divergence canary |
| conflicts.py (174) | REIMPL | /128-in-two-origins detection + START/END + conflicts.jsonl |
| render.py (225) | REIMPL | render wg-mesh.conf + §16.3 non-overlap invariant. NO boat equivalent |
| commands.py (170) | REIMPL (reuse run.Runner) | whole-table `wg syncconf`; load-bearing syncconf-then-set-private-key order |
| state.py (335) | REIMPL (shrinks) | persist ownership stream + own_generation + routable_dead; membership persistence SUBSUMED |
| daemon.py (562) | REIMPL (shrinks) | scan→render→apply; verifier machinery → AliveDelegate/Keyring |
| loop.py (429) | SUBSUMED+REIMPL | peer timers SUBSUMED; **retain** scan→debounced-apply→GC scheduler |
| main.py (286) | REIMPL | entry point → `boat networkd` verb |
| config.py (225) | REIMPL (shrinks) | most timers → memberlist config; app knobs stay |
| identity.py (65) | REIMPL trivial | load identity.json → Config.Name + advertise addr |
| localownership.py (195) | **REUSE** | already ported: internal/netapply/localownership/localownership.go |
| observe.py (85) | REIMPL/REUSE | Counter → internal/metrics |
| sdnotify.py (71) | REIMPL tiny | READY/WATCHDOG/STOPPING to $NOTIFY_SOCKET (~30 LOC, no dep) |
| __init__.py (22) | DROP | package marker |

## 2. Record model
Two kinds (spec §7.1/7.2, records.py:37-135):
- **MembershipRecord** (one per host, origin==host_id, origin-mutable): host_id
  (128-bit UUID = Frappe Server name), kind∈{member,leaving}, state∈{alive,leaving}
  on wire (suspect/dead are observer-local), endpoint (bare public IPv6, no port),
  wg_public_key (base64 Curve25519), mesh_address (`fdaa:0:0:<idx>::1`), generation
  (uint64 monotonic-per-origin), signing_public_key (base64 ed25519). `validate()`
  rejects whitespace/control chars in the 3 render-interpolated fields
  (anti-`[Peer]`-injection) — re-port + call at every parse boundary AND at render.
- **OwnershipAdvertisement** (per-origin FULL set of owned /128s at a generation,
  never a delta): origin, generation, owned (frozenset IPv6), signature. Effective
  table = union of latest advertisement per origin; a /128 in ≥2 origins is a
  CONFLICT — dropped, never elected. Cross-origin generations NEVER compared.

Signing (ed25519 confirmed; Go uses stdlib crypto/ed25519):
1. Per-record sig (§19.3): over canonical JSON of record minus signature + `_kind`
   domain tag ("membership"/"ownership"). Signed by ORIGIN's key.
2. Envelope sig (§19.1): whole-datagram by sender — **GOES AWAY** (memberlist
   authenticates sender via transport + optional AES).
3. Introduction cert (§19.5): operator-signed `{host_id, signing_public_key,
   generation}` for a first-contact newcomer + detached operator sig over seed
   bytes. **Both survive.**
Keys at `/etc/atlas-networkd/{wg-private-key,wg-public-key,signing-private-key,
signing-public-key}` + divergence canary (catches the "controller pushed `****`
mask" partition bug) — re-port the canary.

Mapping onto memberlist's three channels:
- MembershipRecord identity → `Node.Name`=host_id, `Config.Name`=our host_id.
- endpoint+port → `Config.AdvertiseAddr/Port` (bind public IPv6).
- wg_public_key/mesh_address/signing_public_key → **Delegate.NodeMeta()** — small
  signed blob (~140 B < 512 MetaMaxSize), changes rarely.
- membership `generation` → memberlist's own **incarnation number** (same
  origin-bumps-only/refute semantics). Stop carrying our own on the wire; keep
  persisting own_generation only for the OWNERSHIP stream.
- kind=leaving → memberlist.Leave() → NotifyLeave.
- state alive/suspect/dead → SWIM + EventDelegate.
- OwnershipAdvertisement updates → **broadcast queue** GetBroadcasts()+NotifyMsg().
- OwnershipAdvertisement convergence → **push/pull** LocalState()+MergeRemoteState().
- per-record ed25519 sig → rides inside NodeMeta blob AND the broadcast/pushpull
  payload; verified before apply. NOT part of dedupe identity (keys on
  (origin,generation)).

## 3. Delegate design
One `networkd` struct implements `memberlist.Delegate` + `EventDelegate` +
`AliveDelegate` (+ maybe MergeDelegate), owning AppliedState + render/apply seam.
- **NodeMeta(limit)**: signed canonical-JSON `{wg_public_key, mesh_address,
  signing_public_key}` + ed25519 sig by our key. Assert fits 512.
- **NotifyMsg(b)**: decode OwnershipAdvertisement, verify sig against origin's
  cached signing pubkey (from that node's Meta), apply §13.2 monotonic rule, on
  change schedule debounced apply. (= gossip._apply_record minus transport.)
- **GetBroadcasts(overhead,limit)**: backed by TransmitLimitedQueue; enqueue our
  new signed OwnershipAdvertisement when local scan detects a changed owned-set.
- **LocalState(join)/MergeRemoteState(buf,join)**: full ownership stream (sigs
  included); MergeRemoteState verifies each + applies monotonic rule per origin.
  The correctness backstop independent of broadcast delivery.
- **Conflict resolution stays app-logic** (memberlist's ConflictDelegate is about
  same-Name collisions — a DIFFERENT concept). After any apply recompute
  effective_ownership → ConflictTracker → conflicts.jsonl + metric + status.json.
- **Dead peers → EventDelegate**: NotifyLeave→re-render (peer drops);
  NotifyUpdate→re-read Meta, update pubkey cache, re-render; NotifyJoin→re-render.
- **THE ONE THING memberlist does NOT give us — `ownership_grace` (§14.3)**:
  memberlist removes a dead node immediately, but ANCP keeps a dead host's
  ownership /128 routes alive for ownership_grace (60s > suspect_timeout +
  dead_grace) so a late-refuting host doesn't blackhole its VMs. Re-port as an
  app-owned timer: on NotifyLeave stamp dead_at[host_id]; keep rendering that
  origin's [Peer] from routable_dead until `now - dead_at >= ownership_grace`, then
  drop. NotifyUpdate/NotifyJoin clears the timer. **Highest-risk behavioral
  nuance.**

## 4. Render/apply reuse
- render.py → **direct Go port, nothing reusable in boat** (boat never rendered the
  mesh). Pure string fn: sort peers by pubkey, per-peer AllowedIPs = owned /128s ∪
  mesh_address/128, drop /128 landing under >1 peer (§16.3 non-overlap + H2
  mesh_address-collision fold), skip self, re-validate. Keep `_assert_no_input_
  overlap` as a Go test-invariant.
- apply: `internal/netapply/vmnetwork/wireguard.go` is **NOT reusable as-is** — it
  does per-VM INCREMENTAL `wg set peer allowed-ips` on per-tunnel interfaces. ANCP
  mesh apply is the OPPOSITE: whole-table atomic `wg syncconf wg-mesh <(wg-quick
  strip <conf>)` then `wg set wg-mesh private-key <file> listen-port`. Incremental
  `wg set peer` is FORBIDDEN (opens a window where a /128 sits under two peers).
  - Reusable: the run.Runner seam + {}-hole quoting; the commands interface +
    validName/validWireGuardKey/validPath validators; the `bash -c <script>`
    process-substitution idiom.
  - Must build: whole-table apply/bring-up builders, /run/atlas-networkd/
    wg-mesh.conf atomic writer, drift-check, and — **load-bearing** — the
    syncconf-FIRST-then-set-private-key-LAST ordering with the build-time
    assertion. Flipping it clears the interface key and silently kills every
    tunnel; re-port the assertion.
  - sudoers: add wg syncconf / wg set / ip link / ip -6 route lines.

## 5. Timers
memberlist.Config (delete from config.py): gossip_interval→GossipInterval;
gossip_fanout→GossipNodes; probe_interval→ProbeInterval; probe_timeout→ProbeTimeout;
indirect_relays→IndirectChecks; anti_entropy_interval→PushPullInterval;
suspect_timeout→SuspicionMult+SuspicionMaxTimeoutMult (memberlist scales suspicion
by log(N) — this is a MULT not an absolute; document the translation for WAN
fleets); leaving_grace→Leave(timeout); inbound_* flood knobs→dropped.

Still app-owned: ownership_scan_interval (2s, poll local-ownership.json);
**apply_debounce** (200ms, collapses a burst into one syncconf — CRITICAL, without
it a /128 hopping twice yields two syncconfs + transient invalid state);
ownership_grace (60s) + dead_grace (30s) route-retention; advertisement_refresh
(60s re-broadcast own set); wg constants (wg_host_port 51820, mtu 1420, device
wg-mesh) + path layout.

App loop shrinks to: every scan_interval→localownership.Read→on change bump
generation, persist, enqueue broadcast, schedule apply; on debounce
deadline→render+drift-check+syncconf; on any EventDelegate callback→schedule apply;
ownership_grace GC ticker→reap routable_dead. memberlist runs everything else.

## 6. Package layout
```
internal/networkd/
  records.go   signing.go   keys.go   identity.go   seed.go
  state.go     render.go    apply.go  conflicts.go
  delegate.go  events.go    trust.go  daemon.go
  metrics.go   sdnotify.go
```
Reuse internal/netapply/localownership, internal/run, internal/metrics,
internal/paths.
- Verb: `case "networkd"` in cmd/boat/main.go — a LONG-RUNNING daemon (mirrors
  cmd/boat/daemon.go serve shape), SEPARATE process/unit from `boat daemon` (which
  runs as User=boat); networkd needs root/CAP_NET_ADMIN for wg/ip/nft. Spec/33 §3
  already says boat SUPERVISES the networkd unit and reads none of its gossip
  state.
- systemd/boat-networkd.service (replaces atlas-networkd.service):
  ExecStart=/usr/local/bin/boat networkd, Type=notify + WatchdogSec, root or
  AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW, Restart=on-failure RestartSec=2,
  StateDirectory=atlas-networkd, RuntimeDirectory=atlas-networkd. Keep the
  /etc/atlas-networkd/ paths.
- go.mod: `require github.com/hashicorp/memberlist v0.6.0` (pulls go-msgpack,
  go-metrics, miekg/dns, google/btree, sean-/seed, go-multierror,
  go-immutable-radix). Real weight increase against boat's tiny go.mod — a
  deliberate exception (re-porting SWIM by hand is what the directive rejects).
  crypto/ed25519 is stdlib.

## 7. Seed / join
Seed comes from Atlas config ON DISK, not a live query: controller writes
`/etc/atlas-networkd/seed.json` + `seed.json.sig` + `operator-public-key` at
provision. Sole out-of-band trust root: operator-signed list of `{host_id,
endpoint, wg_public_key, signing_public_key, mesh_address, generation}`.
Boot: identity.Load; keys.ensure (wg+ed25519); seed.LoadSeed (**verify operator
detached sig over exact bytes**, fail-closed if key configured + sig bad; build
host_id→signing_public_key trust dir + load operator-public-key); build
memberlist.Config (Name=host_id, AdvertiseAddr=endpoint, timers §5, Delegate/
Events/Alive=our structs, Keyring=cluster PSK if used); `memberlist.Create`;
**`m.Join(["[<seed.endpoint>]:<ancp_port>", …])`** (replaces cold_join entirely);
rejoin loop (retry ~2s if 0 reached + seeds exist; lone host with no seeds comes up
empty); first render+apply (peer-empty is fine).
Newcomer introduction (§19.5): carries introduction-signature; rides in signed
NodeMeta (or first-contact user-msg); existing hosts' AliveDelegate.NotifyAlive
rejects a node whose signing key isn't in the seed trust dir OR backed by a valid
operator introduction cert. TOFU-persist learned keys.

## 8. Risks
1. **Hard-cutover** (§0): during a rolling upgrade the not-yet-upgraded and
   upgraded halves form two DISJOINT clusters; cross-cluster VM↔VM private traffic
   blackholes until the last host flips. Convert per isolated cluster in one
   window. Already-established kernel-WireGuard peers unaffected; no NEW peers cross
   the boundary.
2. **Keep BOTH memberlist encryption AND ed25519 sigs.** memberlist AES Keyring is
   a symmetric cluster PSK — authenticates "a cluster member sent this", does NOT
   bind a specific origin to a record. ANCP's threat model is a
   compromised-but-authenticated host forging another origin's records. So keep the
   ed25519 per-record/per-Meta sigs (verify every OwnershipAdvertisement + peer
   NodeMeta against the ORIGIN's published key before apply); optionally add the
   Keyring for transport integrity + join-gate (also replaces dropped ratelimit
   flood defense). Keep the operator detached seed sig + introduction cert. Subtle:
   memberlist relays a node's Meta on push/pull, so verify Meta sig in
   NotifyAlive/NotifyUpdate against the EXISTING stored key (not the self-asserted
   key) unless first-contact-with-introduction.
3. **Stable shapes for Atlas ingest** (no GET /export in ANCP — the mirror is
   boat's, pulling GET /v1/export; ANCP surfaces are files): local-ownership.json
   `{"owned":[...]}` + lockfile path byte-identical (already guaranteed by the Go
   port); /var/lib/atlas-networkd/status.json JSON shape; conflicts.jsonl
   `{kind,private_ip,origins,at}` line shape; seed.json/.sig/operator-public-key/
   introduction-signature parse + detached-sig-over-bytes scheme EXACT (else hosts
   fail to bootstrap); effective-table semantics (union of latest per origin,
   conflicts dropped never elected) are a BEHAVIORAL contract — a re-port that
   "helpfully" elects a conflict winner silently double-routes tenant traffic.
4. Secondary: ownership_grace (highest behavioral risk); apply_debounce (drop →
   transient two-peer-one-/128); syncconf-then-set-private-key order (flip → kills
   mesh); memberlist suspicion is log(N)-scaled not absolute (document the
   SuspicionMult translation); dependency weight (deliberate one-time exception).

**Bottom line:** re-port records/signing/render/apply/conflicts/seed/keys nearly
verbatim (pure, battle-tested); wire into memberlist via delegate.go (NodeMeta=
signed Meta, NotifyMsg/GetBroadcasts=ownership broadcasts, LocalState/
MergeRemoteState=ownership anti-entropy) + events.go (Join/Leave/Update→re-render +
the app-owned ownership_grace timer). Delete the SWIM/transport/anti-entropy/probe/
flood half. Keep scan→debounce→syncconf as your own loop. Guard the three nuances
memberlist won't: ownership_grace retention, apply_debounce, whole-table syncconf
ordering.
