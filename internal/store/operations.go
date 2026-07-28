package store

import (
	"fmt"
	"time"

	"github.com/frappe/boat/internal/model"
	"go.etcd.io/bbolt"
)

// ClaimOperation is the idempotency primitive this package exists for. In one
// write transaction it either records a fresh Running operation and reports
// claimed=true, or finds the identifier already present and returns that record
// with claimed=false so the caller runs nothing.
//
// bbolt admits one writer at a time, so the read and the write below cannot
// interleave with another claim. Two concurrent posts of one identifier are
// therefore serialised by the file itself: the first records the operation, the
// second reads it back. Exactly one caller is ever told to run the verb, which
// is what turns a retried Atlas Task into a replay instead of a double-run.
//
// An identifier already recorded against a different verb or VM is
// ErrOperationConflict. Replay is only replay when it is the same operation;
// handing a caller someone else's result is worse than refusing them.
//
// The claim also carries this run's incarnation, written by the same
// transaction. That is what makes crash recovery cover all nine verbs and not
// only the ones that go on to record a decision: an operation is in flight from
// the moment it is claimed, and a claim whose stamp were written afterwards
// would leave a window in which a crash strands an operation nothing can see.
func (store *Store) ClaimOperation(identifier, verb, uuid string) (model.Operation, bool, error) {
	var operation model.Operation
	var claimed bool
	err := store.database.Update(func(transaction *bbolt.Tx) error {
		var err error
		operation, claimed, err = store.claimInBucket(transaction.Bucket(operationsBucket), identifier, verb, uuid)
		return err
	})
	if err != nil {
		return model.Operation{}, false, err
	}
	return operation, claimed, nil
}

func (store *Store) claimInBucket(
	bucket *bbolt.Bucket, identifier, verb, uuid string,
) (model.Operation, bool, error) {
	recorded, found, err := getRecord[model.Operation](bucket, identifier)
	if err != nil {
		return model.Operation{}, false, err
	}
	if found && !recorded.Matches(verb, uuid) {
		return model.Operation{}, false, conflictWith(identifier, recorded)
	}
	if found {
		// A replay keeps the incarnation that claimed it. The operation belongs to
		// the run that started it, and re-stamping it here would let a restarted
		// daemon adopt work it never began — which is the one thing the stamp exists
		// to prevent it from doing.
		return recorded, false, nil
	}
	claim := runningOperation(identifier, verb, uuid, store.incarnation)
	return claim, true, putRecord(bucket, identifier, claim)
}

// runningOperation stamps in UTC. Hosts run in whatever timezone they were
// imaged with, and a journal an operator reads across several of them at once
// has to be comparable without knowing which.
func runningOperation(identifier, verb, uuid string, incarnation int64) model.Operation {
	return model.Operation{
		Identifier:         identifier,
		Verb:               verb,
		VirtualMachineUUID: uuid,
		Status:             model.OperationRunning,
		Incarnation:        incarnation,
		StartedAt:          time.Now().UTC(),
	}
}

// CompleteOperation writes the terminal record for an operation the journal has
// already claimed.
//
// The rule over an already-terminal record is first completion wins, silently:
// a late completion for an operation the journal already recorded as finished is
// dropped, and this returns nil. Two reasons. The outcome recorded first is the
// one Atlas was answered with, so overwriting it would leave the journal
// disagreeing with the Task row an operator is reading. And the caller's
// contract — "this operation's outcome is recorded" — is already satisfied, so
// erroring would turn a harmless duplicate into a spurious failure.
//
// The other three cases are loud, because they are caller bugs rather than
// races: a status that is not terminal, an identifier the journal never claimed,
// and an identifier recorded against different work (ErrOperationConflict).
func (store *Store) CompleteOperation(operation model.Operation) error {
	if !operation.Finished() {
		return fmt.Errorf("complete %s: status %q is not terminal", operation.Identifier, operation.Status)
	}
	return store.database.Update(func(transaction *bbolt.Tx) error {
		return completeInBucket(transaction.Bucket(operationsBucket), operation)
	})
}

func completeInBucket(bucket *bbolt.Bucket, operation model.Operation) error {
	claim, found, err := getRecord[model.Operation](bucket, operation.Identifier)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", errUnclaimedOperation, operation.Identifier)
	}
	if !claim.Matches(operation.Verb, operation.VirtualMachineUUID) {
		return conflictWith(operation.Identifier, claim)
	}
	if claim.Finished() {
		return nil
	}
	return putRecord(bucket, operation.Identifier, terminalRecord(claim, operation))
}

// terminalRecord merges the caller's outcome onto the claim already in the
// journal. The claim owns StartedAt — a completion may not rewrite when the work
// began — and the store stamps EndedAt when the caller left it zero, so that no
// finished record in the file is missing its end time.
func terminalRecord(claim model.Operation, outcome model.Operation) model.Operation {
	outcome.StartedAt = claim.StartedAt
	if outcome.EndedAt.IsZero() {
		outcome.EndedAt = time.Now().UTC()
	}
	return outcome
}

// GetOperation reports found=false with a nil error for an identifier the
// journal has never seen. Absence is an answer, not a failure.
func (store *Store) GetOperation(identifier string) (model.Operation, bool, error) {
	var operation model.Operation
	var found bool
	err := store.database.View(func(transaction *bbolt.Tx) error {
		var err error
		operation, found, err = getRecord[model.Operation](transaction.Bucket(operationsBucket), identifier)
		return err
	})
	if err != nil {
		return model.Operation{}, false, err
	}
	return operation, found, nil
}

// ListOperations returns every operation this host has recorded, ordered by
// identifier, so that two restarts read the same work in the same sequence.
//
// This is what makes a crash visible at all. Without it an operation a restart
// left Running is in the file and reachable by name, but unreachable by anyone
// who does not already know the name — so no recovery path can find the very
// records it exists to close (spec/33-boat.md §3.3, §11.5).
//
// It reads the whole bucket, which is never pruned: a host that has run ten
// thousand verbs decodes ten thousand records here. That is paid once per boot
// by the one caller (internal/journal's Unfinished), and the day it stops being
// cheap is the day this bucket needs a retention rule — which is a decision for
// the chapter, not a filter quietly added here.
func (store *Store) ListOperations() ([]model.Operation, error) {
	operations := []model.Operation{}
	err := store.database.View(func(transaction *bbolt.Tx) error {
		return transaction.Bucket(operationsBucket).ForEach(func(key, value []byte) error {
			operation, err := decodeRecord[model.Operation](key, value)
			if err != nil {
				return err
			}
			operations = append(operations, operation)
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("read this host's operations: %w", err)
	}
	return operations, nil
}

// conflictWith says what the identifier is already spoken for, because the whole
// point of refusing is that the caller has confused two pieces of work.
func conflictWith(identifier string, recorded model.Operation) error {
	return fmt.Errorf(
		"%w: %s is recorded as %s of %s",
		ErrOperationConflict, identifier, recorded.Verb, recorded.VirtualMachineUUID,
	)
}
