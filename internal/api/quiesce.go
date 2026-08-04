package api

// quiesce.go is the daemon side of §5 step 3: the host-wide pause a self-update
// needs, which host.go (internal/update) names as the Quiescer seam and says does
// not exist yet. This file is that seam made real, wired from the daemon.
//
// It is three effects in one call, and each is here rather than in internal/update
// because each spans a part of the daemon that package cannot see:
//
//   - An ADMISSION GATE that makes perform refuse to CLAIM new operations. This is
//     the API layer's own state, checked in the one place every verb funnels
//     through (perform, performMigrationPhase), so a quiesced daemon turns a new
//     verb away with a RETRYABLE 409 — no claim, no journal record — rather than
//     booking a terminal failure the caller would read as "the work ran and lost".
//   - A bounded DRAIN of the operations already in flight. Closing the gate stops
//     NEW claims; the operations admitted before it closed are still running inside
//     their reconciler turns, and each finishes by writing its own terminal record
//     (operations.go's record). Draining is waiting for that in-flight count to
//     reach zero, which is exactly "let the operations already in flight reach a
//     journal checkpoint" — their checkpoints are their own terminal records.
//   - A CHECKPOINT record of the quiesce itself, so an operator reading the journal
//     on a host wedged mid-update sees the daemon paused for one, at which version.
//
// # The one limitation, stated out loud
//
// The drain waits for operations ADMITTED THROUGH THE API — the ones that hold a
// claim and owe a terminal record. It does NOT wait for the reconciler's own
// periodic sweep passes, which also run inside Reconciler.Do. That is deliberate:
// a sweep is forward-only and idempotent (internal/reconcile), holds no claim, and
// is safe to be cut short by the restart and re-run by the fresh daemon — so
// counting it would make the reconciler, which is almost never fully idle, hold
// the drain open until the grace expired every time. The in-flight count tracked
// here is therefore the API's admitted operations, which is the set whose
// interruption actually loses work.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/wire"
)

const (
	// defaultDrainGrace bounds how long Quiesce waits for in-flight operations to
	// finish before it gives up and lowers the gate again. It sits at the daemon's
	// own shutdown grace: the same in-flight operations drain on an ordinary stop,
	// so a quiesce that could not drain in this window is one a restart could not
	// either, and reporting it lets the updater abort rather than restart onto a
	// host still mid-verb.
	defaultDrainGrace = 10 * time.Second
	// defaultDrainPoll is how often the drain re-checks the in-flight count. Short
	// enough that a quiesce of an idle daemon returns near-instantly, long enough
	// that the poll itself is not the cost.
	defaultDrainPoll = 25 * time.Millisecond

	// selfUpdateCheckpointVerb and selfUpdateCheckpointUUID name the checkpoint
	// record. The "uuid" is a sentinel, never a VM: it is spelled so the API
	// boundary's IsUUID check would reject it, so it can never collide with a real
	// VM's operations, and it is only ever read back by an operator grepping the
	// journal — no lifecycle path routes on it.
	selfUpdateCheckpointVerb = "self-update-quiesce"
	selfUpdateCheckpointUUID = "self-update"
)

// admissionGate is the quiesce gate as perform consults it. closed is the whole
// of the pause; inFlight is what the drain waits on. Both live under one mutex so
// the enter/close race that would otherwise slip an operation past a closing gate
// cannot happen: enter and close are mutually exclusive, so an operation is either
// admitted-and-counted before the close, or refused after it, never in between.
type admissionGate struct {
	mutex    sync.Mutex
	closed   bool
	inFlight int
}

func newAdmissionGate() *admissionGate { return &admissionGate{} }

// enter admits one operation and counts it in, or refuses when the gate is closed.
// A refused caller made no claim and holds nothing to release — it never reaches
// leave — so the count only ever tracks operations that are genuinely running.
func (gate *admissionGate) enter() bool {
	gate.mutex.Lock()
	defer gate.mutex.Unlock()
	if gate.closed {
		return false
	}
	gate.inFlight++
	return true
}

// leave releases one admitted operation. Paired with a successful enter by a defer
// in perform, so every path out of a verb — success, replay, failure, abandonment
// — decrements exactly once.
func (gate *admissionGate) leave() {
	gate.mutex.Lock()
	defer gate.mutex.Unlock()
	gate.inFlight--
}

// close raises the gate. New enters are refused from here; the operations already
// counted in are left to drain.
func (gate *admissionGate) close() {
	gate.mutex.Lock()
	defer gate.mutex.Unlock()
	gate.closed = true
}

// open lowers the gate. It is the abort-path inverse of close and is idempotent —
// opening an open gate is a no-op — because Resume is called whenever an update
// gives up before the swap, whether or not it ever quiesced.
func (gate *admissionGate) open() {
	gate.mutex.Lock()
	defer gate.mutex.Unlock()
	gate.closed = false
}

// idle reports whether every admitted operation has left.
func (gate *admissionGate) idle() bool {
	gate.mutex.Lock()
	defer gate.mutex.Unlock()
	return gate.inFlight == 0
}

// drain blocks until the gate is idle or ctx ends, polling rather than waiting on
// a condition variable so the wait is bounded by the caller's context with no
// goroutine left behind when it times out. It is only meaningful after close: a
// drain on an open gate could race a fresh enter forever.
func (gate *admissionGate) drain(ctx context.Context, poll time.Duration) error {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		if gate.idle() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Quiescer implements update.Quiescer (its Quiesce/Resume shape) on the running
// daemon. The POST /v1/quiesce and /v1/resume handlers delegate to it; the
// out-of-cgroup updater drives those endpoints over the socket. It composes the
// admission gate, the drain and the journal — the three parts host.go said had to
// be wired here because no single package holds all of them.
type Quiescer struct {
	gate       *admissionGate
	operations OperationStore
	drainGrace time.Duration
	drainPoll  time.Duration
	version    string
	now        func() time.Time
}

// newQuiescer wires a Quiescer over a gate and the operation store, defaulting the
// drain bounds. version is the running build, stamped into the checkpoint so the
// journal says which binary was being replaced.
func newQuiescer(gate *admissionGate, operations OperationStore, version string) *Quiescer {
	return &Quiescer{
		gate:       gate,
		operations: operations,
		drainGrace: defaultDrainGrace,
		drainPoll:  defaultDrainPoll,
		version:    version,
		now:        func() time.Time { return time.Now().UTC() },
	}
}

// Quiesce raises the gate, drains the operations already in flight, and records a
// checkpoint. It returns only once the host is idle enough that a restart replays
// cleanly — that is the contract host.go states for this seam.
//
// A failure LOWERS THE GATE again before returning, on both the drain and the
// checkpoint path. A quiesce that could not complete must leave the daemon
// serving, not half-paused refusing every verb: the updater treats the error as
// its abort signal (apply.go resumes and stops), and a gate left closed would
// otherwise strand the host refusing work with no updater left to reopen it.
func (quiescer *Quiescer) Quiesce(ctx context.Context) error {
	quiescer.gate.close()
	drainCtx, cancel := context.WithTimeout(ctx, quiescer.drainGrace)
	defer cancel()
	if err := quiescer.gate.drain(drainCtx, quiescer.drainPoll); err != nil {
		quiescer.gate.open()
		return fmt.Errorf("in-flight operations did not drain within %s: %w", quiescer.drainGrace, err)
	}
	if err := quiescer.checkpoint(); err != nil {
		quiescer.gate.open()
		return fmt.Errorf("record the self-update checkpoint: %w", err)
	}
	return nil
}

// Resume lowers the gate. It is the strict inverse of Quiesce's close and the
// abort path only: once the update has restarted the daemon a fresh process is
// serving and there is nothing to resume, so this runs just when an update gives
// up before the swap.
func (quiescer *Quiescer) Resume(ctx context.Context) error {
	quiescer.gate.open()
	return nil
}

// checkpoint writes the terminal journal record of this quiesce. It is a claimed-
// then-completed operation because that is the only durable record the store
// admits — a free-standing decision is refused for naming no claimed operation
// (internal/store) — and it is completed at once so it is terminal: a terminal
// record is never handed to the reconciler's crash-resume, so this marker cannot
// be mistaken for interrupted VM work.
//
// The identifier carries the timestamp, so a host quiesced for two updates over
// its life keeps both markers rather than one overwriting the other (first
// completion wins in the store, so a fixed id would freeze the first). Verb and
// output name what an operator needs: that this was a self-update pause, and which
// version was live when it happened.
func (quiescer *Quiescer) checkpoint() error {
	now := quiescer.now()
	identifier := fmt.Sprintf("boat-self-update-%d", now.UnixNano())
	if _, _, err := quiescer.operations.ClaimOperation(identifier, selfUpdateCheckpointVerb, selfUpdateCheckpointUUID); err != nil {
		return err
	}
	return quiescer.operations.CompleteOperation(model.Operation{
		Identifier:         identifier,
		Verb:               selfUpdateCheckpointVerb,
		VirtualMachineUUID: selfUpdateCheckpointUUID,
		Status:             model.OperationSuccess,
		EndedAt:            now,
		Output:             fmt.Sprintf("quiesced for a self-update at %s, running version %s\n", now.Format(time.RFC3339), quiescer.version),
	})
}

// Quiesce is POST /v1/quiesce: pause admission and drain, driven by the updater
// over the socket. A drain that times out or a checkpoint that cannot be written
// is a 500 — the gate is already lowered again by then — so the updater learns the
// host would not go quiet and aborts rather than restarting onto a busy daemon.
func (server *Server) Quiesce(ctx context.Context, request wire.QuiesceRequestObject) (wire.QuiesceResponseObject, error) {
	if err := server.quiescer.Quiesce(ctx); err != nil {
		return internalFault("This host could not quiesce for a self-update.", err), nil
	}
	return wire.Quiesce200JSONResponse{State: wire.QuiesceStateStateQuiesced}, nil
}

// Resume is POST /v1/resume: lower the gate raised by /quiesce. It cannot fail —
// opening the gate is a local flag flip — so it always answers 200, which is what
// makes it safe as apply.go's abort path: the updater can always undo a quiesce.
func (server *Server) Resume(ctx context.Context, request wire.ResumeRequestObject) (wire.ResumeResponseObject, error) {
	if err := server.quiescer.Resume(ctx); err != nil {
		return internalFault("This host could not resume after a self-update.", err), nil
	}
	return wire.Resume200JSONResponse{State: wire.QuiesceStateStateServing}, nil
}

// unavailableWhileQuiescing is the refusal perform hands a new operation while the
// daemon is paused for a self-update. It is a 409 with a stable reason token, not
// a 503, so it rides the conflict path the verbs already document — and the token
// is what tells the caller this 409 MUST be retried (once the new binary serves),
// unlike the identifier-conflict 409 beside it which must not. No claim was made
// and nothing was journalled, so the retry is a first attempt, not a replay.
func unavailableWhileQuiescing() *errorResponse {
	return conflictBecause(wire.ErrorReasonServiceQuiescing,
		"This host is paused for a self-update and is not claiming new operations; retry once it reports the new version.")
}
