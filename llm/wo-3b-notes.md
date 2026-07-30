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
_(appended as each commit lands)_

## Deferred / remaining WO-3b work (for review)
- **The big one: per-VM network-up/down apply** (netns/veth/tap/NAT44/proxy-NDP/
  EUI-64/off-link route/per-VM nft isolation, port of `vm-network-up.py` 306 LOC
  + `vm-network-down.py` 140 LOC + `private_network.py` + `wireguard.py` +
  `firewall.py`). Largest, riskiest module (§3.5). Not started this pass.
- **Customer-gateway host forwarding** (Atlas-computed, Boat-applied).
- **Live-host differential harness** (§3.5): no harness runs Go verb beside the
  Python one on a live machine and diffs host effects. Still owed before any
  network-apply verb cuts over to native-only. Reserved-ip apply is verified at
  unit level only this pass; a live proxy-VM+reserved-IP exercise is NOT done
  (no proxy VM on the boat hosts tonight).
- **Boat hosts run stale builds** (e77b6e0/b1e7279 vs main); redeploy needed to
  exercise anything shipped here on staging.

## Known gaps / things to double-check
_(appended as found)_
