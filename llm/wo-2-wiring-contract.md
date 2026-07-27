# WO-2 wiring contract — binding

WO-2's components are built, tested and **not plugged in**. `cmd/boat`'s `serve`
constructs only the store, the API server, a `vm.Manager` and a `watch.Hub`:
`reconcile.New`, `journal.New`, `adopt.NewScanner` and `park.NewTrap` have zero
non-test callers. This closes that.

Two agents work in parallel. The signatures below are fixed.

## The two rules this wiring exists to make true

1. **A verb never touches the host directly.** Every path that mutates a VM goes
   through `reconciler.Do(ctx, uuid, fn)`, so a verb and a reconcile pass can
   never drive one machine at once. Today an API `stop` and an API `start` for
   one UUID run concurrently on two goroutines; that is reachable with two
   ordinary requests and it interleaves `vm-network-down` with `vm-network-up`.

2. **`desired_power = Stopped` outranks a wake trap.** The rule is implemented in
   exactly one place — `reconcile.plan` — and the trap does not call it. Wiring
   the trap's callback to `vm.Wake` instead would resurrect a stopped VM from an
   unauthenticated SYN and bypass the fence with it.

## internal/reconcile — grow the interface (agent A)

```go
type VirtualMachines interface {
	Start(ctx context.Context, runner *run.Runner, uuid string) (bool, error)
	Stop(ctx context.Context, runner *run.Runner, uuid string, request vm.StopRequest) error
	Sleep(ctx context.Context, runner *run.Runner, uuid string, request vm.SleepRequest) (vm.SleepResult, error)
	Wake(ctx context.Context, runner *run.Runner, uuid string) error
	Observe(ctx context.Context, runner *run.Runner, uuid string) (model.VirtualMachine, error)
}
```

`*vm.Manager` already satisfies all five. Assert it: `var _ VirtualMachines = (*vm.Manager)(nil)`.

`plan()` gains two steps and must stay a pure function:

- **`stepWake`** — a `StatusSleeping` VM whose desired power is Running and which
  was requested. Today `plan` returns `stepStart` for this, and `vm.Start` cannot
  wake anything: the unit carries
  `ConditionPathNotExists=…/sleeping`, so `systemctl start` skips it, exit 0,
  and the trailing `is-active` then fails the pass. The marker has to come off
  first, which is `vm.Wake`. **A test currently asserts the wake works because
  the fake's `Start` sets Running unconditionally — fix that fake.**
- **`stepSleep`** — a Running VM enrolled via `SleepOnIdle` whose idle timeout has
  elapsed. `model.VirtualMachine` needs an observed idleness field for this
  (`LastTrafficAt`), read from `paths.TrafficCounterFile`. If you cannot source
  it without leaving `plan` pure, implement `stepWake` only and say so — do not
  invent an idle signal.

The precedence rule must be checked BEFORE the wake trigger is read, so a
`PowerStopped` VM is never woken however the pass was requested.

## cmd/boat + internal/api — construct and route (agent B)

`serve` builds the whole daemon:

```go
journal  → journal.New(database, <path beside the store>)
reconciler → reconcile.New(database, vm.NewManager(), journal)
trap     → park.NewTrap(runner, func(ctx, uuid) error { reconciler.Wake(uuid); return nil })
scanner  → adopt.NewScanner()
```

- **Adopt at startup, before serving.** A Boat started on a live host must learn
  its VMs by reading the host, and quarantined artifacts must land in the store
  rather than being ingested as VMs.
- **Run the reconciler and the trap** for the daemon's lifetime, and shut both
  down cleanly on SIGTERM before the store closes.
- **`api.Dependencies` gains a `Reconciler`** so handlers can serialize. Every
  verb handler runs its work inside `reconciler.Do(ctx, uuid, …)`. A nil
  Reconciler must be legal for tests, and must NOT silently mean "run
  unserialized" — decide and document what nil means.
- The trap's callback goes through `reconciler.Wake`, never `vm.Wake`.

Watch the shutdown ordering: today `shutdown` closes the database
unconditionally while a verb may still be running, which strands that
operation's record non-terminal forever. Fix that while you are here.
