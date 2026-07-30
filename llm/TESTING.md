# Testing Boat — a guide for future work

Boat is the per-host Go daemon that replaces Atlas's Python host scripts, verb by
verb, and the discipline is: **each Go module must produce byte-identical host
effects to the Python it replaces before it goes live.** This guide is how you
prove that for any piece — the fast unit level, and the live differential on a
real host.

Read this before touching `internal/netapply/` or porting any host verb.

---

## 0. The two test levels, and why both

1. **Golden / recorder unit tests** (no host, milliseconds). Every command a
   module renders is asserted against the exact string the Python renders. This
   catches a wrong flag, a missing `sudo`, a mis-quoted value. It is the gate in
   `make check`. It does NOT prove the command *works* on a host — only that it
   matches the reference.
2. **The live differential** (a real host, the truth). Run the Python and the Go
   for the same input on a staging host, capture the resulting host state
   (`nft list`, `ip netns`, routes, `wg show`), and `diff`. This is what actually
   proves equivalence. `spec/33-boat.md §3.5` calls it the harness that is "owed"
   before any network module cuts over. It is not optional for anything that
   touches `nft`/`ip`/`wg`.

A module is "done" only when BOTH pass. Golden alone has shipped a
rendering that was correct-vs-Python but the Python itself was buggy (see the
`oifname` quoting bug in `wo-3b-notes.md`) — the live diff is what exposes that.

---

## 1. Fast local tests

```sh
cd /home/qwerty/boat
make check                 # gofmt -l + go vet + go test -race ./...   — the gate
go test ./internal/netapply/vmnetwork/   # one package
go test -run TestUpRenders ./internal/netapply/vmnetwork/   # one test
```

`make generate` regenerates `internal/wire` from `api/openapi.yaml` (needed after
any openapi edit); it fetches `oapi-codegen` via `go run`, checks the result in.

**gofmt gotcha:** `gofmt -l <dir>` *lists* unformatted files and still exits 0, so
a `&&` chain hides it. Always run `gofmt -w <dir>` before committing, and never
trust a bare `gofmt -l` in a success chain.

**Diagnostics noise:** the editor's gopls reports `could not import
github.com/frappe/boat/...` and `undefined: <symbol>` constantly — it resolves
against the wrong module. Ignore it. `go build`/`go test` are the truth.

---

## 2. The golden / recorder pattern (how to write a unit test)

Every host-touching package has a `commands` seam (`Run`/`RunUnchecked`/`OK`,
sometimes `Input`/`InstallFile`). Tests inject a `fakeCommands` recorder that
records each rendered command with a prefix — `""` for `Run`, `"- "` for
`RunUnchecked` (tolerated failure), `"? "` for `OK` (a boolean probe) — and
returns scripted output. You then `assertTrace` the exact sequence.

**Render exactly like production.** The recorder's `render` must use
`run.Substitute` (see `vmnetwork/vmnetwork_test.go`), NOT a naive `{}` replace,
because production shell-quotes each parameter. A brace nft clause comes out
single-quoted (`'{ … }'`); a safe value (address, device name, CIDR) comes out
bare. If your golden has the wrong quoting, your render helper is wrong, not the
code.

**Where the goldens come from:** capture them from the Python. For a module with
pure builder functions (`reserved_ip_nat.py`, `private_network.py`,
`firewall.py`, `wireguard.py`), copy the builders into a throwaway `python3`
script backed by `shlex.quote` and print the output — that is your `want`. See
`scratchpad/*_golden.py` history for the exact shape.

Idempotency tests matter most: script the fake so the rules/chains already
exist, then assert NO `add`/`insert` is issued. This is where the `oifname`
quoting bug lived — the guard must match nft's *listed* form.

---

## 3. The live differential (the important one)

The lab is a boat host with no VMs — **host-1 (`168.144.179.179`)**. It runs the
`boat` daemon but manages no guests, so creating a throwaway test VM's networking
and tearing it down disturbs nothing. `ssh -i ~/.ssh/DO root@<ip>`.

### 3.1 Stage the Python reference beside the Go

```sh
# from the atlas boat-split worktree, package the reference scripts + lib:
cd /home/qwerty/atlas/apps/atlas/.claude/worktrees/boat-split
tar czf /tmp/.../atlas-ref.tgz -C scripts vm-network-up.py vm-network-down.py -C lib atlas
scp -i ~/.ssh/DO /tmp/.../atlas-ref.tgz root@168.144.179.179:/root/atlas-ref.tgz
ssh … 'mkdir -p /root/atlas-ref && tar xzf /root/atlas-ref.tgz -C /root/atlas-ref'
# vm-network-up.py imports `atlas.*` from a sibling dir, so the layout must be
#   /root/atlas-ref/{vm-network-up.py, vm-network-down.py, atlas/…}
# sanity: ssh … 'cd /root/atlas-ref && python3 -c "import atlas._run; print(\"ok\")"'

# build + ship the Go binary (do NOT overwrite the running /usr/local/bin/boat):
cd /home/qwerty/boat && make build
scp -i ~/.ssh/DO bin/boat root@168.144.179.179:/root/boat-new
```

### 3.2 Give the test VM a synthetic sidecar

A per-VM sidecar lives at
`/var/lib/atlas/virtual-machines/<uuid>/network.env`. Use a fixed throwaway UUID
(`dead0000-0000-4000-8000-000000000001`) and synthetic-but-valid values so
nothing collides with real infrastructure. A public VM needs `TAP_DEVICE`,
`VIRTUAL_MACHINE_IPV6`, `ATLAS_NETNS`, `HOST_VETH`, `NAMESPACE_VETH`,
`IPV4_HOST_CIDR`, `IPV4_GUEST_CIDR`, `ATLAS_FC_UID`. Add `PRIVATE_ADDRESS` +
`TENANT_PREFIX` for the private plane; add a `firewall.env` and/or a `tunnels/`
dir of `<name>.env` for those. Templates are in the scratchpad
(`test-network.env`, `private-network.env`, `firewall.env`, `wg-test.env`).

### 3.3 Run each, capture, diff — with a CLEAN table between runs

```sh
CAP='echo NETNS; ip netns list | grep dead; echo NFT;
     sudo nft list chain inet atlas forward | grep -E "2001:db8::2|fdaa" | sed "s/^\s*//";
     echo ROUTES; ip -6 route show 2001:db8::2; ip -4 route show 100.64.200.10;
     echo NDP; ip -6 neigh show proxy | grep 2001:db8::2'

# Python:
ssh … "sudo nft delete table inet atlas 2>/dev/null; cd /root/atlas-ref &&
        python3 vm-network-up.py <uuid> >/dev/null 2>&1; $CAP" > py.txt
# Go (flush first, so both start from the SAME empty state):
ssh … "python3 /root/atlas-ref/vm-network-down.py <uuid> >/dev/null 2>&1;
        sudo nft delete table inet atlas 2>/dev/null;
        /root/boat-new vm-network-up <uuid> >/dev/null 2>&1; $CAP" > go.txt
diff py.txt go.txt && echo IDENTICAL
```

**`sudo nft delete table inet atlas` between runs is mandatory.** Teardown
deliberately leaves host-wide scaffold (the masquerade, the `fdaa::/16` terminal
drop) in place, so a Python-up-then-Go-up comparison without flushing shows a
benign *ordering* difference (a leftover terminal drop sits at its old position).
That is a test artifact, not a bug — flush and it is byte-identical. Confirm any
"difference" is really an ordering-of-disjoint-rules artifact before calling it a
bug.

### 3.4 Command-trace differential (for a rendering check on a real host)

Both the Python `run()` and the Go `run.Runner` trace `+ <command>` lines to
stderr. Run each with `2>trace.txt` and compare the mutating lines
(`grep -E '^\+ (sudo|ip )' | grep -v '^+ ('`). Expect the Go to have a few EXTRA
read lines the Python doesn't (Go reads `network.env` via `sudo cat`; the Python
reads it with a plain in-process `open`, untraced; Go's `OK` probes trace, the
Python's `run_ok` doesn't) — those are reads with no host effect. The MUTATING
lines must match.

### 3.5 Always tear down and leave the host clean

```sh
ssh … '/root/boat-new vm-network-down <uuid> >/dev/null 2>&1
       sudo ip link del wg-testtun 2>/dev/null      # tunnels: down leaves the iface
       sudo nft delete table inet atlas 2>/dev/null # remove test scaffold
       rm -rf /var/lib/atlas/virtual-machines/<uuid> /root/atlas-ref /root/boat-new'
# verify: netns gone, no dead* dirs, boat.service still active.
```
Never leave a netns, a wg interface, an nft table, or the ref/binary behind.
Verify `systemctl is-active boat.service` at the end — you must not disturb the
running daemon (that is why the Go binary goes to `/root/boat-new`, not
`/usr/local/bin/boat`).

---

## 4. Per-component recipes

| Piece | Unit test | Live differential |
|---|---|---|
| `sidecar` env writers | golden vs `network_env.py` upsert/remove | n/a (pure) |
| `localownership` | temp-dir read/add/remove, `-race` for the flock | n/a (pure file); interop: read a Python-written `{"owned":[…]}` |
| `reservedip` render + apply | golden nft/ip strings; recorder both delivery models | needs a proxy VM + a reserved IP + DO anchor metadata — **do NOT** test the anchor path on host-1 (its DNAT would catch the host's own anchor traffic). Routed path only, or a dedicated host. |
| `vmnetwork` public plane | `TestUpRenders…` full sequence | §3.3 — proven byte-identical |
| `vmnetwork` private plane | `private_test.go` golden + idempotency | §3.3 with `PRIVATE_ADDRESS`+`TENANT_PREFIX`; security-critical, always diff live |
| `vmnetwork` firewall | `firewall_test.go` | §3.3 with a `firewall.env` |
| `vmnetwork` wireguard | `wireguard_test.go` | needs `apt install wireguard-tools` on the host (already on host-1) + a `wg genkey`; diff `wg show` + the input/forward rules |
| lifecycle verbs (`vm`, `api`) | `internal/vm/*_test.go`, `internal/api/*_test.go` recorder | drive through `boat vm <verb>` against the daemon on a host with a real VM (meo, `168.144.146.52`, has one) — but that VM is live, do not disturb it |

The nine lifecycle verbs already have a differential precedent (`spec/33 §3.5`
lists what was cut over on unit-level + manual exercise). New verbs follow the
same two-level bar.

---

## 5. Testing the Atlas (Python) side

The Atlas changes live on branch `feat/boat-split`, in the worktree
`apps/atlas/.claude/worktrees/boat-split` (the main bench sits on stale `main`;
do NOT switch it). Boat-related tests:

```sh
# from a bench whose apps/atlas is on feat/boat-split:
bench --site <site> run-tests --module atlas.tests.test_boat_client
bench --site <site> run-tests --module atlas.tests.test_boat_mirror
```

`test_boat_client.py` mocks `requests.request` and asserts the Task row + the
request body per verb — extend it when you add a BoatClient method (see the
`reserved_ip` cases). **Running the suite against the worktree needs a bench
pointing at it**, which the laptop bench does not; a boat-split staging bench
does. Until then, `python3 -m py_compile` + pattern-match against a tested verb is
the floor, and the gap must be stated (it was, for the reserved-ip routing).

Fake-backed servers never make a real Boat/SSH call (`is_fake_server`), so a
dev/test fleet exercises the Task-row bookkeeping without a host.

---

## 6. The staging fleet (and what NOT to touch)

- **host-1 `168.144.179.179`** — boat, no VMs. The differential lab. Safe.
- **host-2 `168.144.209.248`** — boat, no VMs.
- **`168.144.146.52` ("meo")** — a live Firecracker VM, no boat. Read its real
  `network.env` for realistic inputs; do NOT disrupt its VM.
- **press-v2 hosts 4/5/6** (`168.144.183.5`, `206.189.129.235`,
  `165.232.187.213`) — a DIFFERENT staging fleet. **Do not touch.**

`ssh -i ~/.ssh/DO root@<ip>`. Reach the user via ntfy (see the
`ntfy-intervention-channel` memory) for anything needing a decision.

---

## 6a. Testing the storage layer (when you port `vm-disk-up` / `lvm`)

The network lab needs only a synthetic `network.env`. Storage needs a real thin
pool, and the boat hosts have none (`sudo vgs` is empty). Build a throwaway
**loop-backed thin pool** so the differential does not touch real storage:

```sh
ssh … 'truncate -s 2G /root/thintest.img
        LOOP=$(losetup --find --show /root/thintest.img)
        pvcreate $LOOP && vgcreate <atlas-vg-name> $LOOP
        lvcreate -L 1G -T <atlas-vg-name>/<atlas-pool-name>
        # then create a thin snapshot LV named as ThinPool.vm_disk(uuid) expects'
```

Match the VG/pool/LV **names** `lvm.py:ThinPool` derives (read them from the
source — do not guess). Then run the Python `vm-disk-up.py <uuid>` and the Go,
and diff `dmsetup ls`, `lvs -o+lv_active`, and `ls -l <jail>/rootfs.ext4` (the
mknod'd node's major:minor). Tear down: `lvremove`, `vgremove`, `losetup -d`,
`rm the img`. **Storage is the §3.5 "LVM CoW ordering" risk — never cut a disk
verb over on golden tests alone; the loop-pool differential is mandatory.**

## 7. Gotchas learned the hard way

- **nft quotes interface names on list** (`oifname "eth0"`). An idempotency guard
  that matches the *unquoted* form never fires → duplicate rules every restart.
  Match a quote-stripped listing, or the quoted text. (This was a real latent bug
  in `vm-network-up.py`.)
- **nft canonicalises addresses on list** (`fdaa:0:0::/48` → `fdaa::/48`). Send
  the un-canonical form in the command, match the canonical form in the guard —
  `private.go` does exactly this.
- **Teardown leaves host-wide scaffold** (masquerade, terminal drop, the `input`
  chain). That is intentional; account for it (flush between diffs).
- **`RunUnchecked` errors only when a command can't run at all** (missing binary,
  cancelled ctx), not on a non-zero exit — that is the `check=False` contract.
- **The reserved-ip anchor path is unsafe to test on a shared host**: its DNAT
  matches the droplet's own anchor IP. Use the routed model or an isolated host.
