package journal

import (
	"fmt"

	"github.com/frappe/boat/internal/model"
)

// Unfinished lists the operations a crash left non-terminal. The reconciler
// resumes these on startup, before it reconciles anything else.
//
// "Unfinished" is not "not finished yet", and the distinction is the whole
// point. An operation this daemon is running right now is also non-terminal, and
// resuming it would double-run the verb this package exists to make replayable —
// two starts of one VM, or a terminate racing its own retry. So an operation
// qualifies only when both of these hold:
//
//   - internal/store, which owns status, says it has not reached a terminal
//     state; and
//   - it was claimed by an EARLIER incarnation of this daemon, so the goroutine
//     that owned it cannot still exist.
//
// The incarnation is what makes the second test exact instead of a guess, and
// store.Incarnation says why a timeout is not an alternative.
//
// Every claimed operation is stamped, whether or not it goes on to decide
// anything, and that is deliberate: what a crash strands is the CLAIM. The seven
// verbs that choose nothing still leave a Running record behind, still answer a
// replay with a non-terminal status, and are still work no process is doing.
// Stamping only alongside a decision would make this list cover rebuild and
// nothing else, which is a crash-recovery path that recovers from almost no
// crashes.
func (journal *Journal) Unfinished() ([]model.Operation, error) {
	operations, err := journal.store.ListOperations()
	if err != nil {
		return nil, fmt.Errorf("read the operations this host has claimed: %w", err)
	}
	unfinished := []model.Operation{}
	for _, operation := range operations {
		if stranded(operation, journal.store.Incarnation()) {
			unfinished = append(unfinished, operation)
		}
	}
	return unfinished, nil
}

// stranded reports an operation no live goroutine can still be running: the
// store never recorded a terminal state for it, and the run of this daemon that
// claimed it has ended.
func stranded(operation model.Operation, incarnation int64) bool {
	return !operation.Finished() && operation.Incarnation < incarnation
}
