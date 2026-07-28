package store

import (
	"fmt"

	"go.etcd.io/bbolt"
)

// incarnationKey is the meta bucket's count of how many times this store has
// been opened. Spelled out, like the observed epoch beside it, so that `strings`
// over the file answers "how many times has this daemon started" without a tool.
const incarnationKey = "incarnation"

// Incarnation is which run of this daemon holds the store open.
//
// Every operation is stamped with it at claim, and the stamp is the whole of how
// an operation a crash abandoned is told apart from one that is merely slow: a
// process that has ended cannot be running work claimed under the number this
// process was handed when it opened the file. The obvious alternative is a
// timeout — treat an operation older than N as lost — and every value of N is
// wrong: short enough to recover a crash promptly is short enough to abandon a
// long migration underneath itself, and long enough to be safe leaves a crashed
// host unrecovered for as long as N. internal/journal's Unfinished is the reader.
func (store *Store) Incarnation() int64 { return store.incarnation }

// beginIncarnation advances the run counter and returns the number this run
// holds.
//
// The read-modify-write below needs no lock of ours, for the reason
// bumpObservedEpoch gives: bbolt admits one write transaction at a time. It is a
// stored counter rather than a process-lifetime random number because the
// comparison Unfinished makes — "earlier than mine" — needs an order and not
// just an identity.
func beginIncarnation(database *bbolt.DB) (int64, error) {
	var incarnation int64
	err := database.Update(func(transaction *bbolt.Tx) error {
		previous, _, err := getRecord[int64](transaction.Bucket(metaBucket), incarnationKey)
		if err != nil {
			return err
		}
		incarnation = previous + 1
		return putRecord(transaction.Bucket(metaBucket), incarnationKey, incarnation)
	})
	if err != nil {
		return 0, fmt.Errorf("begin a store incarnation: %w", err)
	}
	return incarnation, nil
}
