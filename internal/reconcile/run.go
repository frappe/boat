package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/frappe/boat/internal/model"
)

// Run drives every VM toward its desired state until ctx ends, and resumes the
// operations a crash left unfinished before it does anything else.
//
// The order is the point. A half-finished terminate sitting in the journal is
// work with a decision already recorded — an address allocated, a volume
// created — and a fresh reconcile pass that ran first would drive that VM toward
// a desired state the terminate is in the middle of retiring, so the two would
// fight: the pass starts the unit the terminate has just torn the disk out from
// under. Resuming first means the interrupted work reaches its own conclusion,
// and only then does the steady-state loop get an opinion.
//
// Returning cancels this reconciler for good: every pass still in flight is
// cancelled with it, and passes requested afterwards are dropped. A daemon being
// replaced by a new binary must stop driving units before the new one starts.
func (reconciler *Reconciler) Run(ctx context.Context) error {
	defer reconciler.stop()
	if err := reconciler.resume(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(reconciler.sweepInterval)
	defer ticker.Stop()
	for {
		reconciler.sweep()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// resume drives the VM behind each operation a crash left unfinished, one at a
// time and on this goroutine, so that all of it is done before the first sweep
// is posted.
//
// A failing resume does not stop the rest. The alternative — return the error
// and never start the loop — means one VM whose host state cannot be read keeps
// every other VM on this host unreconciled, which turns a single wedged guest
// into a host-wide outage. Reading the journal at all is different: a journal
// that cannot be read means this daemon does not know what it was doing when it
// died, and starting a reconciler in that state is the fighting scenario above.
func (reconciler *Reconciler) resume(ctx context.Context) error {
	unfinished, err := reconciler.journal.Unfinished()
	if err != nil {
		return fmt.Errorf("read the operations left unfinished: %w", err)
	}
	for _, operation := range unfinished {
		reconciler.resumeOne(ctx, operation)
	}
	return nil
}

// resumeOne converges one interrupted operation's VM.
//
// The pass is a sweep-flavoured one: a resume is not somebody asking for this VM
// by name, so it converges power and leaves a sleeping VM asleep. The decisions
// the operation recorded are counted into the log rather than replayed here —
// re-entering a verb at its checkpoint is the verb's own business, and this
// package would have to know what every verb means to do it for them.
func (reconciler *Reconciler) resumeOne(ctx context.Context, operation model.Operation) {
	log := logger(operation.VirtualMachineUUID).With("operation", operation.Identifier, "verb", operation.Verb)
	decisions, err := reconciler.journal.Decisions(operation.Identifier)
	if err != nil {
		log.Error("could not read the decisions of an unfinished operation", "error", err)
		return
	}
	log.Warn("resuming a virtual machine whose operation was left unfinished", "decisions", len(decisions))
	if err := reconciler.pass(ctx, operation.VirtualMachineUUID, triggerSweep); err != nil {
		log.Error("could not resume a virtual machine after a restart", "error", err)
	}
}

// sweep posts a pass for every VM Atlas has asserted state for.
//
// It is the safety net rather than the driver — a verb that changes desired
// state wakes its VM immediately — and what it catches is everything that never
// produced an event: a wake lost to a restart, a unit that died on its own, a
// desired record written while this daemon was down. Posting is all it does, so
// one slow VM cannot delay the sweep of the rest.
func (reconciler *Reconciler) sweep() {
	records, err := reconciler.store.ListDesired()
	if err != nil {
		// Logged and not returned: the store failing to list is a reason to try
		// again on the next tick, not to take the reconciler down and leave every
		// VM on this host unreconciled.
		slog.Error("could not read the desired state to sweep", "error", err)
		return
	}
	for _, record := range records {
		reconciler.request(record.UUID, triggerSweep)
	}
}
