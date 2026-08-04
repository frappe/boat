# WO-4 — cross-host migration port: implementation map

Blueprint for porting Atlas's cross-host VM migration saga from Python
(`atlas/atlas/migration.py` + `scripts/migration-*.py`, driven over SSH `run_task`)
to Boat RPCs. Target contract: `spec/33-boat.md §8` + boat's verb/fence/run
conventions. Citations are `file:line` in the boat-split worktree.

## 0. Who drives, how a phase runs
- **Atlas is the saga orchestrator; each host runs a stateless idempotent script
  per phase.** `migration.py` is the controller-side driver. The migration row's
  `status` IS the phase cursor; the driver re-derives what to do from `doc.status`
  (`advance_migration`, migration.py:180-209), never from memory.
- **Two drivers:** `start_migration` (migration.py:125-159) runs one phase inline
  via `run_task`, commits the new `status`, re-enqueues itself until terminal.
  `reconcile_migrations` (migration.py:110-122) is a `*/2 * * * *` cron re-entering
  stranded rows. Both re-entrant because every phase is idempotent.
- Each phase = `run_task(server, script="migration-…", variables=…)` via
  `_run_phase_task` (migration.py:1253-1265). On a boat host this becomes
  `run_boat_task` — an **HTTP RPC to the named host's Boat** (boat_client.py:24,
  spec/33:1101-1102).

## 1. Ordered saga phase table
`PHASE_ORDER` (migration.py:69-80) is authoritative:
`Pending → ExportingSnapshot → TargetPreparing → InjectingIdentity →
CutoverStarting → Hydrating → CollapseClone → Repointing → Cleanup → Done`.
(Boot-then-hydrate puts `Hydrating` AFTER `CutoverStarting` so the copy is off the
downtime clock.)

> Gotcha: the doctype `status` Literal (virtual_machine_migration.py:55-66) is
> STALE — lists Hydrating before CutoverStarting, omits CollapseClone. Port
> against `PHASE_ORDER`, and fix the Literal.

| # | Phase | Host | Script | Host-side commands (extracted) | Idempotency |
|---|---|---|---|---|---|
| 1 | Pending | source | `_phase_pending` (mig.py:346) | `vm.stop(memory_snapshot=False, stop_timeout_seconds=3, graceful=False)` then `db_set(has_memory_snapshot=0)`. Discards RAM (worthless cross-host). | no-op if already Stopped |
| 2 | ExportingSnapshot | source | migration-export-source.py | `mkdir -p /var/lib/atlas/run`; LVM thin-snap `atlas-snap-<uuid>-migrate` (+`atlas-datasnap-…`); `ss -ltn 'sport = :<port>'`; `qemu-nbd --persistent --read-only --cache=none --bind=<src-ipv4> --port=<port> --pid-file=<pf> --fork <dev>` (root=`nbd_port`, data=`nbd_port+1`); `cat <pf>`. Guard `too_full_to_snapshot`. Emits nbd_port/nbd_pid/root_size/data_size. | reuses snapshot + listening qemu-nbd |
| 3 | TargetPreparing | target(+source ship) | `_phase_target_preparing`(mig.py:402)→clone-target.py PHASE=prepare | Base ship (LOCAL images only): export-base(source)→receive-base prepare(target)→poll-hydration→receive-base finalize. Clone: `modprobe nbd`,`modprobe dm_clone`,`which nbd-client`; pool data_percent>=80 guard; `create_thin` root(+data); `_ensure_nbd_client`(`nbd-client -N "" <host> <port> /dev/nbd<slot> -persist`); `_ensure_dm_clone`(meta LV+`dd if=/dev/zero … bs=1M count=16`, `dmsetup create <name> --table "0 <sectors> clone <meta> <dest> <nbd> 32768"`). keep-address: `_bring_up_forward_tunnel`. | every step probes artifact; dm-clone over DEAD nbd is removed+rebuilt |
| 4 | InjectingIdentity | target(+DB) | migration-inject-identity.py | change-address `allocate_ipv6(target)` (row for_update); keep-address reuse old /128; `dmsetup info <clone-basename>` (fallback plain LV `test -b`); `inject_identity(device, Identity, regenerate_host_keys=False)` — mounts the CLONE, writes network.env+authorized_keys, PRESERVES host keys. | rewrites identical files |
| 5 | CutoverStarting | target+source **DOWNTIME ENDS** | `_phase_cutover_starting`(mig.py:735) | (0) withdraw-private-source (`remove_local_owned(private /128)`, cache only). (1) keep-address `_install_forward_routes`: forward-up both→target-receive(`ip -6 route replace default dev <tun> table <t>`;`ip -6 rule add from <vmv6> lookup <t> priority 100`)→source-forward(`ip -6 route replace <vmv6>/128 dev <tun>`;nft forward accept rules;`ip -6 neigh replace proxy <vmv6> dev <uplink>`). (2) provision-vm(target) `CLONE_ROOTFS_DEVICE=/dev/mapper/atlas-vm-<uuid>-clone` — boots on read-through clone. (3) `_finalize_cutover` DB: server=target, ipv6=new, status=Running. | all idempotent; source copy intact through Hydrating |
| 6 | Hydrating | target **poll-only** | `_phase_hydrating`(mig.py:680)→poll-hydration.py | `dmsetup info <clone>`(root+`-data`); `_clone_source_alive`(`dmsetup table` field5 src maj:min, `/sys/block/nbdN/pid`, `/proc/<pid>`); `dmsetup message <clone> 0 enable_hydration`; `dmsetup status` → `<hydrated>/<total>` → percent; reports MIN across disks. | re-enter until 100%; dead-source→re-run prepare; 30 no-progress ticks→Failed |
| 7 | CollapseClone | target | migration-cutover-target.py | `dmsetup info`; skip if already `linear`; fully-hydrated guard; **transparent collapse**: `dmsetup suspend`;`dmsetup reload <clone> --table "0 <sectors> linear <dest> 0"`;`dmsetup resume` (keeps maj:min so FC's open fd survives; `remove` would fail busy); `_disconnect_nbd`; `_remove_meta`(`lvremove -f`). | linear/missing → no-op |
| 8 | Repointing | **Atlas/DB — point of no return** | `_phase_repointing`(mig.py:925) | `_finalize_cutover`; change-address→`_repoint_routes`(Subdomain db_set+reconcile_proxies); `_handle_reserved_ip`; `_repoint_private_plane`(no-op). **`boot_epoch` must bump here per §8 — NOT implemented today.** | idempotent |
| 9 | Cleanup | source | migration-cleanup-source.py | `_kill_nbd` ports nbd_port..+3; `rm -f /var/lib/atlas/run/migrate-base-meta-*.tar`; `lvremove` `-migrate` snaps; `systemctl disable --now <unit>`; `vm-network-down <uuid>`; `rm -rf <vm-dir>`; pool disk `.remove()`. keep-address: tunnel/route/NDP left up permanently. | best-effort; **row IS the backstop** (no orphan-LV reconciler) |

Not a phase: `collapse_forward` (mig.py:1120-1235) — operator teardown of a
keep-address forward via forward-down.py both hosts + re-provision on fresh /128.

## 2. Fencing (the CORE of WO-4 — currently inert)
"Repoint requires positive source fencing" (spec/33:1115-1118): don't advance to
Repoint until an acked heartbeat from the target at the new epoch AND the source is
fenced (epoch bump acked, or source confirmed Unknown). Epoch bumps at exactly one
point: Repoint.

- `boot_epoch`: per-UUID monotonic int, Atlas sole issuer, mirrored into each
  Boat's fence bucket on every desired PUT (spec §11.1). First epoch = 1.
- Built in boat: `store.SetFenceEpoch` (refuses regression), `fence.Allow`,
  `api.assert` writes fence before desired record. Boot gate `refuseUnfenced` runs
  at start-vm/wake-vm (NOT stop/resume/pause/reserved-ip).
- **The gap to fill:**
  1. Epoch comparison is a tautology — PUT writes fence+desired from one doc, so
     `heldEpoch == record.BootEpoch` always; `ErrStaleEpoch` unreachable
     (internal/api/fence.go:50-57).
  2. No `server` field in `model.DesiredVirtualMachine` — `server == self` from
     §11.1 uncheckable.
  3. Nothing in Atlas bumps `boot_epoch` beyond 1.
- WO-4 must: (a) make Repoint issue a HIGHER boot_epoch, PUT to target; source's
  DELETE retraction then leaves it stale/no-authority; (b) add `server` to the
  desired doc; (c) fence the CUTOVER boot on the target (today boots via
  provision-vm, an UNFENCED path).

## 3. S3 sync — "Atlas never proxies bytes" (spec/33:1122-1124)
Atlas presigns + owns S3 creds; Boat transfers via the presigned URL.
- Upload (upload-snapshot-s3.py): host holds NO S3 creds. Per object: `zstd -q -f
  -3 -T0 -o <temp> <src>`; `sha256sum`; `stat -c %s`; `curl --fail --upload-file
  <temp> <presigned-PUT>`; `rm`.
- Restore (restore-snapshot-s3.py): presigned GET. Per object: `curl --output
  <temp> <url>`; **verify sha256 before decompress** (`sha256sum -c -`); block →
  recreate clean thin LV + `zstd -d --sparse -o <lv> <temp>`; `sync`.

## 4. Warm fan-out
- warm-snapshot-vm.py (producer): pause vCPUs (`firecracker_api PATCH /vm
  Paused`); `PUT /snapshot/create` writes vmstate.bin+mem.bin inside the jail;
  LVM thin-snap disk at same instant; resume in finally; move pair to durable dir
  + host-signature.json; remove source jail's snapshot dir.
- promote-snapshot-image.py (same-server, bytes never leave): `dd` snapshot-LV →
  read-only `atlas-image-<name>` LV; image dir with kernel hard-linked + rootfs
  sentinel.
- Cross-host fan-out of a LOCAL base = the migration base-ship over NBD; each
  target Boat validates `host_signature` before restore.

## 5. Proposed Boat surface
New package `internal/migration/` (one file per phase, pure logic unit-testable no
host). New endpoints `/vms/{uuid}/migrate/{phase}` in api/openapi.yaml then `make
generate` (NEVER hand-edit internal/wire). Each new privileged binary (qemu-nbd,
nbd-client, dmsetup, dd, socat, systemd-run, modprobe, nft, ip, tar, zstd, curl,
sha256sum) needs an enumerated sudoers.d/boat line — no wildcard.

| Python script | Boat RPC | internal fn | Reuse |
|---|---|---|---|
| export-source | POST …/migrate/export-source | migration.ExportSource | vmdisk, run(qemu-nbd) |
| export-base | …/migrate/export-base | ExportBase | vmdisk/image LV, run |
| clone-target(prepare) | …/migrate/clone-target | CloneTarget | vmdisk.CreateThin, run |
| receive-base | …/migrate/receive-base (body phase) | ReceiveBase | vmdisk, run |
| inject-identity | …/migrate/inject-identity | InjectIdentity | vm/identity.go, vm/volume.go (mount CLONE, regen_host_keys=false) |
| poll-hydration | GET …/migrate/hydration | PollHydration | run(dmsetup) |
| cutover-target(collapse) | …/migrate/collapse-clone | CollapseClone | run(dmsetup suspend/reload/resume), vmdisk |
| forward-up | …/migrate/forward-up (body role) | ForwardUp | run(systemd-run socat), netapply |
| source-forward | …/migrate/source-forward | SourceForward | netapply firewall/private |
| target-receive | …/migrate/target-receive | TargetReceive | netapply |
| forward-down | …/migrate/forward-down | ForwardDown | netapply/down.go |
| withdraw-private-source | …/migrate/withdraw-private | WithdrawPrivate | **netapply/localownership** reuse |
| cleanup-source | …/migrate/cleanup-source | CleanupSource | vm/terminate.go + vmdisk + boat vm-network-down |
| cutover boot | extend boot path | vm.Start/Rebuild + CloneRootfsDevice | vm boot; **must be fence-gated** |

- Pending = existing stop-vm verb (graceful=false, stop_timeout_seconds=3).
- Repoint stays in Atlas (Subdomain/proxy DB + epoch bump). Boat only accepts the
  raised epoch via PUT and the source DELETE retraction (both work today).
- S3/warm: POST …/snapshot/upload-s3, restore-s3, warm-snapshot,
  POST /images/{name}/promote — same `perform` shape.

## 6. Topology
**Atlas orchestrates phase-by-phase in a star; the two hosts never talk on the
control plane** — Atlas issues a separate RPC to the named server each phase. Only
direct host↔host channels are DATA plane (Boat opens, doesn't proxy bytes):
- NBD (disk + base ship): source qemu-nbd binds public IPv4, target nbd-client
  dials it over plain TCP (stage-1 unencrypted).
- socat forward tunnel (keep-address): per-VM tun bridged host↔host over TCP.

No shared state — all pure functions of the UUID: `nbd_port = 10000 + uuid%5000`;
`nbd_base_slot = (uuid%4)*4` (max 4 concurrent target migrations = 16 nbd devices);
tunnel device/port/table via `derive_vm_tunnel*`. **Boat must derive these
identically on both ends** (put in internal/paths or a shared migration helper).
The reconciler already serialises verbs per-UUID via `reconciler.Do(ctx, uuid,
fn)` — the right home for per-VM phase actors.

## 7. Gotchas (port-breakers)
1. Doctype status Literal stale — port against PHASE_ORDER.
2. **Fence is inert today** (§2) — the core of WO-4.
3. **Cutover boot is unfenced** — route it through a fenced boot path that also
   carries the clone-rootfs device.
4. Detached processes = systemd transient units, not daemon children. qemu-nbd
   `--fork` returns after socket ready (exec-and-wait OK); socat uses `systemd-run
   --unit=atlas-mig6-<port>` so it survives daemon restarts. Boat must systemd-run
   these.
5. **`nbd-client -check` LIES** — reports connected off a stale kernel binding
   after the client died. Read liveness from `/sys/block/nbdN/pid` + `/proc/<pid>`.
   Do NOT simplify to `-check`.
6. **Transparent dm-clone collapse is load-bearing.** `dmsetup remove` on a clone
   FC holds open fails "busy"; collapse is suspend→reload clone→linear→resume,
   keeping maj:min so the guest's rootfs fd survives.
7. **Hydrating is a poll that also mutates** (enable_hydration). `perform`
   journals one terminal record per operation_id — a per-tick poll needs a fresh
   op-id each tick or (cleaner) a non-journaled GET.
8. Idempotency keys off HOST ARTIFACTS not the store (dmsetup info, ss, pidfiles,
   /sys/block/nbdN/pid). Use run.Probe/run.OK — but OK collapses "couldn't probe"
   into "no", only safe for guarding a mutation; a REPORTED percent/health must use
   three-valued Probe.
9. Shell metacharacters: glob `rm -f …migrate-base-meta-*.tar` and `tar -xf <nbd>`
   need run.Shell, not run.Run.
10. Base-ship only for LOCAL images (`_image_is_local` = no rootfs_url). Syncable
    images use sync-image and must NOT be shipped.
11. Data disk = second parallel dm-clone; poll reports MIN. Slots: root=base+0,
    data=base+1, base-image=base+2, meta-tar=base+3. Preserve 4-slots-per-migration
    or concurrent migrations collide on /dev/nbdN.
12. Cleanup has no reconciler backstop — "the row IS the backstop". Boat's export
    DOES surface LogicalVolume.Origin + Quarantined — a better backstop; consider
    using it.
13. keep-address leaves tunnel+source-forward+proxy-NDP up permanently after
    Cleanup; only collapse_forward tears down. proxy-NDP re-assert is UNCONDITIONAL
    on every provider (don't gate on forward_address/DigitalOcean).
14. cleanup-source still shells legacy `/var/lib/atlas/bin/vm-network-down.py` — on
    a boat host this must become boat's own `vm-network-down`.
15. Plain-TCP data path on public IPv4 is unencrypted (stage-1). Keep the seam so
    a future WireGuard/SSH carrier drops in without touching phase logic.
