package journal

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/frappe/boat/internal/model"
	"go.etcd.io/bbolt"
)

// inFlight is the journal's index of the operations that have recorded a
// decision, and of which incarnation of this daemon recorded their last one.
//
// The operation's status is deliberately not copied here. internal/store owns
// status; a second copy would be a second truth, and the two would disagree
// exactly when a crash landed between the two writes.
type inFlight struct {
	OperationID string    `json:"operation_id"`
	Incarnation int64     `json:"incarnation"`
	At          time.Time `json:"at"`
}

// markInFlight stamps the operation with the incarnation that just decided
// something for it, replacing any earlier stamp.
//
// Replacing rather than keeping the first is what makes a resume resumable more
// than once: an operation picked up by incarnation 5 and abandoned again is
// stamped 5, so incarnation 6 still sees it as stranded. Keeping the original
// stamp would work too, right up until a resume crashes.
func markInFlight(transaction *bbolt.Tx, decision Decision, incarnation int64) error {
	record := inFlight{OperationID: decision.OperationID, Incarnation: incarnation, At: decision.At}
	return putRecord(transaction.Bucket(operationsBucket), decision.OperationID, record)
}

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
//   - the last decision it recorded was recorded by an EARLIER incarnation of
//     this daemon, so the goroutine that owned it cannot still exist.
//
// The incarnation is what makes the second test exact instead of a guess. The
// obvious alternative is a timeout — treat an operation older than N as lost —
// and every value of N is wrong: short enough to recover a crash promptly is
// short enough to resume a long migration underneath itself, and long enough to
// be safe leaves a crashed host unrecovered for as long as N. A process that is
// gone cannot have taken a decision under the number this process was handed
// when it opened the file, so "slow" and "crashed" stop being a judgement call.
//
// What this does not cover, said plainly: an operation that crashed before
// recording any decision is not listed, because the journal never heard of it.
// It authorized no non-idempotent choice — that is what recording first buys —
// so there is nothing to replay, and the VM it touched is converged by the
// reconciler's ordinary forward pass. What is left behind is its Running record
// in the store, and the fix for that is internal/store learning to list its own
// operations (see the package comment).
func (journal *Journal) Unfinished() ([]model.Operation, error) {
	stranded, err := journal.stranded()
	if err != nil {
		return nil, err
	}
	unfinished := []model.Operation{}
	for _, identifier := range stranded {
		operation, running, err := journal.stillRunning(identifier)
		if err != nil {
			return nil, err
		}
		if running {
			unfinished = append(unfinished, operation)
		}
	}
	return unfinished, nil
}

// stranded lists the operations whose last decision belongs to an earlier
// incarnation, in key order, so that two restarts of one host resume the same
// work in the same sequence.
func (journal *Journal) stranded() ([]string, error) {
	identifiers := []string{}
	err := journal.decisions.View(func(transaction *bbolt.Tx) error {
		return transaction.Bucket(operationsBucket).ForEach(func(key, value []byte) error {
			record, err := decodeRecord[inFlight](key, value)
			if err != nil {
				return err
			}
			if record.Incarnation < journal.incarnation {
				identifiers = append(identifiers, record.OperationID)
			}
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("read the journal's in-flight operations: %w", err)
	}
	return identifiers, nil
}

// stillRunning asks the store, which is the authority on an operation's status.
func (journal *Journal) stillRunning(identifier string) (model.Operation, bool, error) {
	operation, found, err := journal.operations.GetOperation(identifier)
	if err != nil {
		return model.Operation{}, false, fmt.Errorf("read operation %s: %w", identifier, err)
	}
	if !found {
		// A decision filed against an operation the store never claimed is a caller
		// bug — every operation is claimed before it runs. It is reported and skipped
		// rather than resumed: there is no record to drive forward, and inventing one
		// would put work into the journal that Atlas never asked for. Failing the
		// whole call instead would strand the resume of every other operation on it.
		slog.Warn("a journalled decision names an operation this host never claimed", "operation", identifier)
		return model.Operation{}, false, nil
	}
	return operation, !operation.Finished(), nil
}
