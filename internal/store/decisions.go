package store

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/frappe/boat/internal/model"
	"go.etcd.io/bbolt"
)

// separator ends the operation identifier inside a decision's key. It is the one
// character an identifier may not contain, and RecordDecision refuses one that
// does: a decision filed under a key nothing can seek to is a decision that was
// not recorded, and a crash is exactly when that would be discovered.
const separator = "/"

// errUndecidableOperation means a decision named an operation that is not in a
// position to take one — never claimed, or already finished. Both are caller
// bugs rather than states a host reaches on its own.
var errUndecidableOperation = errors.New("operation is not running and cannot record a decision")

// RecordDecision writes a decision durably, in the same transaction as the check
// that the operation naming it is still entitled to take one.
//
// Durability is bbolt's commit fsync, which is why this package never sets
// NoSync: a decision still sitting in a buffer has not been made, and a caller
// that acted on one is a caller whose retry chooses differently. The write-ahead
// rule this serves — record, THEN do the thing — belongs to internal/journal,
// which is the entry point a verb calls.
//
// Reading the operation and appending the decision in one transaction is
// spec/33-boat.md §11.5's "commit in one transaction" as far as one file takes
// it today: a decision can never end up in this bucket naming work this host was
// not doing. It is also the reason the decisions live here and not in a file of
// their own — bbolt takes an exclusive lock per file, so two files could not do
// this at all.
func (store *Store) RecordDecision(decision model.Decision) error {
	if err := usable(decision); err != nil {
		return err
	}
	// Stamped in UTC, like the operation records beside it: hosts run in whatever
	// timezone they were imaged with, and a journal read across several of them at
	// once has to be comparable without knowing which.
	if decision.At.IsZero() {
		decision.At = time.Now().UTC()
	}
	return store.database.Update(func(transaction *bbolt.Tx) error {
		if err := deciding(transaction, decision.OperationID); err != nil {
			return err
		}
		return appendDecision(transaction, decision)
	})
}

// usable refuses a decision the store could not hand back. Both refusals are
// silent corruption if they are allowed through: an unnamed step cannot be found
// by the code resuming at it, and an identifier carrying the separator files its
// decisions where another operation's prefix scan will read them.
func usable(decision model.Decision) error {
	switch {
	case decision.OperationID == "":
		return errors.New("a decision has to name the operation that took it")
	case strings.Contains(decision.OperationID, separator):
		return fmt.Errorf("operation identifier %q may not contain %q", decision.OperationID, separator)
	case decision.Step == "":
		return fmt.Errorf("the decision recorded for operation %s has to name its step", decision.OperationID)
	}
	return nil
}

// deciding refuses a decision whose operation was never claimed or has already
// finished. Every operation is claimed before it runs and a finished one decides
// nothing more, so either is a bug in the caller — and letting it through would
// leave a decision no replay can ever reach, filed against work nobody did.
func deciding(transaction *bbolt.Tx, operationID string) error {
	operation, found, err := getRecord[model.Operation](transaction.Bucket(operationsBucket), operationID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", errUnclaimedOperation, operationID)
	}
	if operation.Finished() {
		return fmt.Errorf("%w: %s is already %s", errUndecidableOperation, operationID, operation.Status)
	}
	return nil
}

// appendDecision writes one entry under a key that sorts after every entry
// already there.
func appendDecision(transaction *bbolt.Tx, decision model.Decision) error {
	bucket := transaction.Bucket(decisionsBucket)
	sequence, err := bucket.NextSequence()
	if err != nil {
		return fmt.Errorf("number the decision for operation %s: %w", decision.OperationID, err)
	}
	return putRecord(bucket, decisionKey(decision.OperationID, sequence), decision)
}

// decisionKey keeps the operation identifier greppable in the file and orders an
// operation's decisions by when they were taken.
//
// The sequence is the bucket's own monotonic counter rather than the timestamp,
// so replay order never depends on a clock: two decisions taken inside one
// millisecond still come back in the order they were made, and a host whose
// clock steps backwards does not reorder its own history. Zero-padded because
// bbolt sorts keys as bytes, and "10" sorts before "9" otherwise.
func decisionKey(operationID string, sequence uint64) string {
	return fmt.Sprintf("%s%s%020d", operationID, separator, sequence)
}

// Decisions returns what an operation already decided, in the order it decided
// it. An operation that decided nothing gets an empty slice and no error: that
// is the answer a first attempt is entitled to, and the same answer a replay
// gets for a step it had not reached yet.
func (store *Store) Decisions(operationID string) ([]model.Decision, error) {
	decisions := []model.Decision{}
	err := store.database.View(func(transaction *bbolt.Tx) error {
		var err error
		decisions, err = decisionsFor(transaction, operationID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read the decisions of operation %s: %w", operationID, err)
	}
	return decisions, nil
}

// decisionsFor seeks to the operation's first key and walks while the prefix
// holds, which is a scan of that operation's entries and of nothing else.
func decisionsFor(transaction *bbolt.Tx, operationID string) ([]model.Decision, error) {
	decisions := []model.Decision{}
	prefix := []byte(operationID + separator)
	cursor := transaction.Bucket(decisionsBucket).Cursor()
	for key, value := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, value = cursor.Next() {
		decision, err := decodeRecord[model.Decision](key, value)
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	return decisions, nil
}
