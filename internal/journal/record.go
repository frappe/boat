package journal

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

// separator ends the operation identifier inside a decision's key. It is the one
// character an identifier may not contain, and Record refuses one that does: a
// decision filed under a key nothing can seek to is a decision that was not
// recorded, and a crash is exactly when that would be discovered.
const separator = "/"

// Record writes the decision durably. It returns only once the decision survives
// a crash — a decision still sitting in a buffer has not been made, and a caller
// that acted on one is a caller whose retry chooses differently.
//
// Durability is bbolt's commit fsync, which is why this package never sets
// NoSync (bolt.go). The decision and the in-flight stamp below land in one
// transaction, so a resumed operation can never find its decision without the
// index entry that says the operation owns it.
//
// The order this is used in is the entire rule: record, then do the thing. A
// decision recorded after its side effect is not a write-ahead journal, it is a
// log — and a log cannot answer the only question a replay has, which is what
// the first attempt chose.
func (journal *Journal) Record(decision Decision) error {
	if err := usable(decision); err != nil {
		return err
	}
	// Stamped in UTC, like the store's operation records: hosts run in whatever
	// timezone they were imaged with, and a journal read across several of them at
	// once has to be comparable without knowing which.
	if decision.At.IsZero() {
		decision.At = time.Now().UTC()
	}
	return journal.decisions.Update(func(transaction *bbolt.Tx) error {
		if err := appendDecision(transaction, decision); err != nil {
			return err
		}
		return markInFlight(transaction, decision, journal.incarnation)
	})
}

// usable refuses a decision the journal could not hand back. Both refusals are
// caller bugs rather than states a host can reach on its own, and both are
// silent corruption if they are allowed through: an unnamed step cannot be found
// by the code resuming at it, and an identifier carrying the separator files its
// decisions where another operation's prefix scan will read them.
func usable(decision Decision) error {
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

// appendDecision writes one entry under a key that sorts after every entry
// already there.
func appendDecision(transaction *bbolt.Tx, decision Decision) error {
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
// it, so a resumed operation re-enters at its checkpoint rather than at its
// beginning.
//
// An operation that decided nothing gets an empty slice and no error: that is
// the answer a first attempt is entitled to, and it is the same answer a replay
// gets for a step it had not reached yet.
func (journal *Journal) Decisions(operationID string) ([]Decision, error) {
	decisions := []Decision{}
	err := journal.decisions.View(func(transaction *bbolt.Tx) error {
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
func decisionsFor(transaction *bbolt.Tx, operationID string) ([]Decision, error) {
	decisions := []Decision{}
	prefix := []byte(operationID + separator)
	cursor := transaction.Bucket(decisionsBucket).Cursor()
	for key, value := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, value = cursor.Next() {
		decision, err := decodeRecord[Decision](key, value)
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	return decisions, nil
}
