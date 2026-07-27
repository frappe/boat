# WO-1 package contract — binding

Same rules as `wo-0-contract.md`: these signatures are fixed, several agents
are writing against them at once, and a signature you believe is wrong goes in
your report rather than getting silently changed.

WO-1 makes Boat the truthful observer of its host, and Atlas a mirror of it.
Authority does not flip in this work order — Atlas's DB is still authoritative,
and everything Boat reports is advisory until WO-2.

Already written, not yours to change: `internal/wire` (generated),
`internal/model` (now also `DesiredVirtualMachine`, `DesiredPower`, `HostFacts`,
`UnitLiveness`, `LogicalVolume`, `Export`, `Quarantine`), `internal/version`,
`internal/paths`, `internal/run`, `internal/vm`.

---

## internal/store — additions

Two new buckets, `desired` and `fence`, plus a monotonic observed epoch.

```go
func (store *Store) PutDesired(record model.DesiredVirtualMachine) error
func (store *Store) GetDesired(uuid string) (model.DesiredVirtualMachine, bool, error)
func (store *Store) ListDesired() ([]model.DesiredVirtualMachine, error)

// SetFenceEpoch refuses to move an epoch backwards and returns
// ErrFenceRegression when asked to. An epoch that can go backwards is not a
// fence.
func (store *Store) SetFenceEpoch(uuid string, epoch int64) error
func (store *Store) FenceEpoch(uuid string) (int64, bool, error)
func (store *Store) FenceEpochs() (map[string]int64, error)

// ObservedEpoch is bumped by every write that changes observed state, in the
// same transaction as that write. A snapshot and its epoch must never disagree.
func (store *Store) ObservedEpoch() (int64, error)

// Snapshot materializes everything an export needs inside ONE short read
// transaction and returns before anything is written out. Never hold the write
// lock across a stream: a busy host that stops answering looks partitioned, and
// a host wrongly declared partitioned is one Atlas stops scheduling onto.
func (store *Store) Snapshot() (model.Export, error)

var ErrFenceRegression = errors.New("...")
```

`PutVirtualMachine` must now bump the observed epoch in its own transaction.

## internal/adopt — port the enumerators of `scripts/reset-server.py`, inverted

`reset-server.py` enumerates every artifact class on a host in order to destroy
them. The same enumeration, read instead of executed, is how Boat learns what is
already on a host it has just been started on.

```go
package adopt

type Result struct {
	VirtualMachines []model.VirtualMachine
	Quarantined     []model.Quarantine
	LogicalVolumes  []model.LogicalVolume
	Units           []model.UnitLiveness
}

type Scanner struct{ /* unexported */ }

func NewScanner() *Scanner

// Scan reads the host and reconstructs what it holds. It never mutates.
func (scanner *Scanner) Scan(ctx context.Context, runner *run.Runner) (Result, error)
```

Enumerate as `reset-server.py` does: VM directories, `firecracker-vm@` units,
network namespaces, atlas links, proxy-NDP entries, atlas LVs. Cross-check the
Firecracker API socket for liveness.

**Quarantine, not a guess.** A UUID whose artifacts disagree — a unit with no VM
directory, an LV with no unit, a half-removed jail — goes to `Quarantined` with
the evidence that made it ambiguous. It must never appear in `VirtualMachines`.
A crash part-way through a terminate is exactly this state, and ingesting it as
a live VM is how a controller boots a VM whose disk it already released.

## internal/fcattach — re-attach to a running Firecracker

```go
package fcattach

type Process struct {
	UUID      string
	Pid       int
	APISocket string
}

// Find locates a live Firecracker for uuid by its deterministic per-UUID API
// socket, confirming liveness through the socket rather than by pattern-matching
// a process table.
func Find(ctx context.Context, runner *run.Runner, uuid string) (Process, bool, error)
```

This is the single most load-bearing capability in the whole split: **re-attach,
never restart**. A Boat that restarts Firecracker on its own startup would kill
every VM on the host every time it is upgraded. Auto-update (WO-5b) is hard-gated
on this working.

## internal/hostfacts — port `scripts/lib/atlas/hostfacts.py` + `server-facts.py`

```go
package hostfacts

func Read(ctx context.Context, runner *run.Runner) (model.HostFacts, error)
```

Live facts, not a bootstrap snapshot: capacity that drifts silently is capacity
that overcommits.

## internal/watch — SSE fan-out

```go
package watch

type Event struct {
	Kind          string    `json:"kind"`   // "virtual-machine" | "operation"
	UUID          string    `json:"uuid"`
	ObservedEpoch int64     `json:"observed_epoch"`
	At            time.Time `json:"at"`
	Payload       any       `json:"payload"`
}

type Hub struct{ /* unexported */ }

func NewHub() *Hub

// Publish never blocks on a slow subscriber. A subscriber that cannot keep up
// is dropped, and its client reconnects and re-reads the export — freshness is
// what a watcher loses, never truth.
func (hub *Hub) Publish(event Event)

func (hub *Hub) Subscribe() (events <-chan Event, cancel func())

// ServeStream writes events to w in text/event-stream framing until ctx ends.
func (hub *Hub) ServeStream(ctx context.Context, w http.ResponseWriter) error
```

Must be race-clean under `go test -race` with concurrent publish and subscribe.

## internal/fence — the boot gate

```go
package fence

// Allow reports whether this host may boot uuid.
//
// It refuses when Boat holds no epoch at all: a Boat that lost its store and
// boots everything it finds on disk is the single most dangerous state in the
// system, because the same VM may already be running on the host it migrated
// to. Empty fence store means boot nothing until Atlas re-asserts.
func Allow(heldEpoch int64, held bool, requestedEpoch int64) error

var ErrNoFence = errors.New("...")
var ErrStaleEpoch = errors.New("...")
```

## internal/api — the three WO-1 handlers

Replace `internal/api/wo1_stub.go` entirely.

- `PutVirtualMachine` stores desired state and the fence epoch, refusing a
  regression with 409. It does not start or stop anything in WO-1.
- `GetExport` returns `store.Snapshot()` enriched with live host facts.
- `Watch` streams from the hub.
- Start must now consult `fence.Allow` and refuse a VM it holds no epoch for.
