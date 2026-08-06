// Package reconcile drives each VM's observed state toward the state Atlas
// desires, with exactly one actor per VM.
//
// # The rule the package exists for
//
// A verb never touches the host directly: it mutates desired state, and the
// reconciler acts. Two drivers on one VM is how a stop lands between a start's
// boot and its readiness check, and the state that leaves behind is not one
// either of them intended — a unit systemd is still starting while the netns it
// needs has just been torn down, and neither operation is wrong on its own. So
// exactly one goroutine may run a verb or a reconcile pass for a given UUID at a
// time, and everything else for that UUID queues behind it (spec/33-boat.md
// §11.3). Do is that serialization point; Wake and Run both go through it.
//
// One actor per VM, not one actor per host: a UUID's work is serialized against
// itself and against nothing else. A host runs dozens of VMs and a start that
// waits for a boot is measured in seconds, so a global lock would make one slow
// guest the whole host's latency.
//
// # Forward-only
//
// Every pass runs toward desired state and is safe to re-enter, which is the
// discipline Atlas already encodes for migration (PHASE_ORDER and
// advance_migration in atlas/atlas/migration.py) generalized to every VM: read
// what the host is now, compare it with what is wanted, take the one step that
// closes the gap, and leave the rest to the next pass. No step may assume it is
// the first attempt, nothing is unwound, and a pass interrupted anywhere is
// corrected by the pass after it. The failure mode this rules out is the one a
// controller with in-memory progress has: a worker dies mid-sequence and the
// remaining steps exist only in a goroutine that is gone.
package reconcile

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/frappe/boat/internal/journal"
	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/store"
	"github.com/frappe/boat/internal/vm"
)

// VirtualMachines is the mechanics the reconciler drives. It is the consumer's
// slice of vm.Manager: five methods, so a test drives a whole reconciler with
// no systemd and no Firecracker under it.
//
// Start and Wake are both here and they are not interchangeable. A sleeping VM
// carries a marker the unit reads as ConditionPathExists=! condition, so `systemctl
// start` skips the unit and exits 0 with the guest still down; only Wake takes
// the marker off first. A reconciler holding just Start can converge every VM on
// this host except the ones sleep-on-idle parked.
type VirtualMachines interface {
	Start(ctx context.Context, runner *run.Runner, uuid string) (bool, error)
	Stop(ctx context.Context, runner *run.Runner, uuid string, request vm.StopRequest) error
	Sleep(ctx context.Context, runner *run.Runner, uuid string, request vm.SleepRequest) (vm.SleepResult, error)
	Wake(ctx context.Context, runner *run.Runner, uuid string) error
	Observe(ctx context.Context, runner *run.Runner, uuid string) (model.VirtualMachine, error)
}

// The one implementation outside tests, asserted here rather than discovered at
// the call site: this package is where the shape is decided, so this is where a
// vm.Manager that drifted out of it has to fail to compile.
var _ VirtualMachines = (*vm.Manager)(nil)

// How often Run sweeps every VM it holds desired state for, and how a failing
// VM's next attempt is spaced.
//
// The sweep is a safety net rather than the driver: a verb that changes desired
// state calls Wake and the pass happens at once, and this interval is what
// catches the VM whose wake was lost, whose unit died on its own, or whose
// desired state changed while this daemon was being restarted. Half a minute is
// the same order as Atlas's own two-minute migration cron, scaled down because
// this one costs a `systemctl show` per VM rather than an SSH round trip.
//
// The backoff is per VM and doubles: a VM that cannot start — no fence, a broken
// image, a full pool — costs this host one attempt per interval instead of a
// core, and the interval grows until a human is in the loop. It is capped so
// that a VM which becomes startable again is picked up in minutes rather than
// hours, and the sweep re-requests it regardless.
const (
	defaultSweepInterval = 30 * time.Second
	defaultBaseBackoff   = 2 * time.Second
	defaultMaxBackoff    = 5 * time.Minute
)

// Reconciler drives this host's VMs toward desired state.
type Reconciler struct {
	store    *store.Store
	machines VirtualMachines
	journal  *journal.Journal

	// lifetime is cancelled when Run returns, and it is the context every pass
	// spawned by Wake runs under. Without it a shutdown would return from Run
	// while passes it started kept driving units, which is the one thing a daemon
	// being replaced by a new binary must not do.
	lifetime context.Context
	stop     context.CancelFunc

	// mutex guards actors, and nothing else. It is never held while a pass runs:
	// the map is looked up in a few instructions and released, or one VM's boot
	// would be every other VM's queue.
	mutex  sync.Mutex
	actors map[string]*actor

	sweepInterval time.Duration
	backoff       backoff
	// wait is the seam over sleeping. A test replaces it to read the delays the
	// reconciler asked for instead of living through them.
	wait func(ctx context.Context, delay time.Duration)

	// observed is told of every observation a pass writes, so a change the
	// reconciler noticed on its own — a guest that died, a unit that failed, a VM
	// the wake trap resumed, anything the sweep catches — reaches the watch stream
	// the same way a post-verb observation does. Without it the stream would carry
	// only what a verb caused, i.e. only what Atlas already knows. It defaults to a
	// no-op and is never nil: a reconciler with no publisher is legitimate — every
	// test in this package drives one, and the daemon wires it only after the API
	// server that owns the hub exists.
	observed func(model.VirtualMachine)
}

// New builds a reconciler over this host's store, its VM mechanics and its
// journal. It starts nothing: Run drives the loop, and Wake and Do work before
// and after it.
func New(database *store.Store, machines VirtualMachines, record *journal.Journal) *Reconciler {
	lifetime, stop := context.WithCancel(context.Background())
	return &Reconciler{
		store:         database,
		machines:      machines,
		journal:       record,
		lifetime:      lifetime,
		stop:          stop,
		actors:        map[string]*actor{},
		sweepInterval: defaultSweepInterval,
		backoff:       backoff{base: defaultBaseBackoff, max: defaultMaxBackoff},
		wait:          sleep,
		observed:      func(model.VirtualMachine) {},
	}
}

// OnObserved wires the reconciler's observations to a publisher, so a change it
// noticed without a verb still announces itself on /watch. The daemon points this
// at the API server's watch publisher once that server is built; a reconciler
// left unwired announces nothing, which is what this package's tests rely on.
//
// It is set once at assembly, before Run or any Wake starts a pass, so no pass is
// ever mid-observe while it changes — the reconciler does not guard it with a lock
// because nothing races it.
func (reconciler *Reconciler) OnObserved(publish func(model.VirtualMachine)) {
	reconciler.observed = publish
}

// Do runs fn as that VM's actor, so a verb and a reconcile pass can never drive
// one VM at the same time. This is the serialization point the whole package
// exists for, and every path into the host — a verb, a sweep, a wake — reaches
// the host through it.
//
// It blocks until this VM's actor is free, because that wait is the queueing:
// abandoning the turn would hand the caller a choice between racing the current
// actor and doing nothing. The wait is bounded by the work already in flight for
// this one UUID, never by another VM's.
func (reconciler *Reconciler) Do(ctx context.Context, uuid string, fn func(context.Context) error) error {
	actor := reconciler.actorFor(uuid)
	actor.turn.Lock()
	defer actor.turn.Unlock()
	// Checked after the wait rather than before it: a caller that queued behind a
	// two-minute boot and was cancelled meanwhile must not then drive the host.
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(ctx)
}

// Wake asks for a pass over one VM. It never blocks and never runs the pass on
// the caller's goroutine: an HTTP handler that flipped desired_power must return
// to Atlas immediately, not wait for a guest to boot.
//
// A pass asked for by name is also how the wake reflex reaches the reconciler,
// which is why it is spelled this way: a VM asleep with a memory snapshot staged
// is resumed by a pass requested for it, and left alone by the periodic sweep, so
// that sleep-on-idle survives the loop that would otherwise undo it. What it can
// never do is start a VM whose desired power is Stopped — see plan.
//
// Requests coalesce. Ten SYNs a second on a sleeping VM are one pass, because a
// pass reads the state at the moment it runs rather than the moment it was asked
// for, and a queue of identical work is a queue that only delays the answer.
func (reconciler *Reconciler) Wake(uuid string) {
	reconciler.request(uuid, triggerRequest)
}

// request posts a pass to a VM's actor and, when no goroutine is already serving
// that actor, starts one. The pass therefore always runs on a goroutine of this
// package's making, never on the caller's.
func (reconciler *Reconciler) request(uuid string, why trigger) {
	actor := reconciler.actorFor(uuid)
	if actor.request(why) {
		go reconciler.serve(actor)
	}
}

// actorFor returns the VM's actor, creating it on first use. Actors are created
// and never removed: one small struct per VM this host has heard of, against a
// removal that would have to prove no goroutine is about to take the lock it is
// deleting.
func (reconciler *Reconciler) actorFor(uuid string) *actor {
	reconciler.mutex.Lock()
	defer reconciler.mutex.Unlock()
	existing, found := reconciler.actors[uuid]
	if found {
		return existing
	}
	created := &actor{uuid: uuid}
	reconciler.actors[uuid] = created
	return created
}

// sleep is the real wait: a timer the shutdown can interrupt, so a daemon told
// to stop does not sit out a five-minute backoff first.
func sleep(ctx context.Context, delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// logger names the VM in every line this package writes, because the question
// asked of a reconciler log is always "what was it doing to this UUID".
func logger(uuid string) *slog.Logger { return slog.With("uuid", uuid) }
