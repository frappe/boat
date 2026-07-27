# WO-2 package contract — binding

Same rules as the earlier contracts: signatures are fixed, several agents write
against them at once, and a signature you believe is wrong goes in your report.

WO-2 is where Boat stops being an observer and starts being the thing that
decides. Verbs mutate desired state; a reconciler drives observed toward it; the
wake reflex becomes resident in the daemon. Per-VM authority becomes flippable
(`observed_authority = Boat`) and reversible.

---

## The rule that shapes every package here

**A verb never touches the host directly. A verb mutates desired state, and the
reconciler acts.**

Two drivers racing on one VM is how a stop lands between a start's boot and its
readiness check, and the resulting state is not one either of them intended. So:
**one actor per VM.** Exactly one goroutine may run a verb or a reconcile pass
for a given UUID at a time, and everything else for that UUID queues behind it.

The corollary worth stating: `desired_power = Stopped` **outranks a wake trap**.
A VM an operator stopped is not resurrected by a stranger's SYN.

## internal/journal — write-ahead decisions

```go
package journal

// Decision is a non-idempotent choice recorded BEFORE the side effect it
// authorizes: which address was allocated, which LV was created, which host slot
// was taken. On a crash-then-retry the replay reads the decision instead of
// making a second, different one.
type Decision struct {
	OperationID string            `json:"operation_id"`
	Step        string            `json:"step"`
	Values      map[string]string `json:"values"`
	At          time.Time         `json:"at"`
}

type Journal struct{ /* unexported */ }

func New(store *store.Store) *Journal

// Record writes the decision durably. It must return only once the decision
// survives a crash — a decision that is still in a buffer has not been made.
func (journal *Journal) Record(decision Decision) error

// Decisions returns what an operation already decided, in order, so a resumed
// operation re-enters at its checkpoint rather than at its beginning.
func (journal *Journal) Decisions(operationID string) ([]Decision, error)

// Unfinished lists operations left non-terminal by a crash. The reconciler
// resumes these on startup; an operation that is merely slow is not unfinished.
func (journal *Journal) Unfinished() ([]model.Operation, error)
```

## internal/park — port `scripts/lib/atlas/park.py` and `scripts/atlas-wake-trap.py`

Read both Python files completely before writing. Their module docstrings are
the specification for this package and explain mechanics you will not guess:

- why a sleeping VM's `/128` routes out a shared always-up dummy (`atlas-park0`)
  — an off-link route makes an inbound packet **forwarded** rather than
  input-delivered and consumed by the host;
- why the rule matches `tcp flags syn / fin,syn,rst,ack` — nft's mask/value
  form, matching only a genuine new-connection SYN, and implying TCP so that
  ping and UDP never wake a VM;
- why the SYN is **dropped, not rejected** — the client retransmits after its
  RTO of about a second, by which time the guest is up and the retransmit lands
  on a live VM;
- why the counter is **named** — only named counters appear in `nft list
  counters`, the flat cheap surface a per-second poll can afford;
- why the name is a pure function of the UUID — `wake_<uuid-without-dashes>`,
  because nft identifiers forbid `-`, and a derived name means the counter-to-VM
  map needs no stored state to survive a restart.

```go
package park

// CounterName and UUIDForCounter are inverses. Derivation, not storage, is what
// lets the map survive a daemon restart with nothing persisted.
func CounterName(uuid string) string
func UUIDForCounter(counter string) (string, bool)

func EnsureDevice(ctx context.Context, runner *run.Runner) error
func Park(ctx context.Context, runner *run.Runner, uuid string, address string) error
func Unpark(ctx context.Context, runner *run.Runner, uuid string, address string) error

// Counters reads every wake counter in one call. Untraced: a per-second poll
// that traced itself would flood the journal and bury the rare real event.
func Counters(ctx context.Context, runner *run.Runner) (map[string]int64, error)

// Trap is the resident reflex: poll the counters, and on the first SYN for a
// still-sleeping VM, wake it locally — no database consulted, because the
// decision is answerable from the host alone.
type Trap struct{ /* unexported */ }

func NewTrap(runner *run.Runner, wake func(ctx context.Context, uuid string) error) *Trap

// Run polls until ctx ends. On startup it re-sweeps park state from the on-disk
// markers: a sleeping VM's unit is suppressed on reboot, so nothing else
// rebuilds its park state and an unswept VM is unreachable forever.
func (trap *Trap) Run(ctx context.Context, interval time.Duration) error
```

Keep the Python's split: rule and argv construction are pure functions, testable
with no host; only the apply functions touch it.

## internal/reconcile — one actor per VM

```go
package reconcile

// VirtualMachines is the mechanics the reconciler drives.
type VirtualMachines interface {
	Start(ctx context.Context, runner *run.Runner, uuid string) (bool, error)
	Stop(ctx context.Context, runner *run.Runner, uuid string, request vm.StopRequest) error
	Observe(ctx context.Context, runner *run.Runner, uuid string) (model.VirtualMachine, error)
}

type Reconciler struct{ /* unexported */ }

func New(store *store.Store, machines VirtualMachines, journal *journal.Journal) *Reconciler

// Run drives every VM toward its desired state until ctx ends, and resumes
// operations a crash left unfinished before it does anything else.
func (reconciler *Reconciler) Run(ctx context.Context) error

// Wake asks for a pass over one VM. It never blocks and never runs the pass on
// the caller's goroutine — the per-VM actor owns that.
func (reconciler *Reconciler) Wake(uuid string)

// Do runs fn as that VM's actor, so a verb and a reconcile pass can never drive
// one VM at the same time. This is the serialization point the whole package
// exists for.
func (reconciler *Reconciler) Do(ctx context.Context, uuid string, fn func(context.Context) error) error
```

Forward-only: every pass runs toward desired state and is safe to re-enter. No
step may assume it is the first attempt.

## internal/vm — the remaining verbs

Port the rest of the lifecycle from `scripts/`, each keeping its Python
reasoning: `pause-vm.py`, `resume-vm.py`, `sleep-vm.py`, `resize-vm.py`,
`rebuild-vm.py`, `terminate-vm.py`, `snapshot-vm.py`, `snapshot-stop-vm.py`.

`sleep-vm.py` has a hard precondition worth carrying over exactly: it refuses
outright when the wake trap is not running. A VM that sleeps with nothing
watching its counter never wakes — better to decline the sleep than to strand it.

Every verb idempotent, every verb one operation, every verb re-runnable.
