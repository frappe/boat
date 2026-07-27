package store

import "go.etcd.io/bbolt"

// observedEpochKey is the one key in the meta bucket. It is a name an operator
// can find with `strings` over the file, because "what epoch does this host
// think it is at" is the first question asked of a host whose export Atlas has
// started refusing.
const observedEpochKey = "observed-epoch"

// ObservedEpoch is this host's count of the changes it has made to its own
// observed state. It only ever goes up, and it is what a caller CASes against.
//
// Zero means this host has never recorded an observation — a fresh store, or a
// store that was lost. That is the same signal an empty fence bucket gives, and
// it is deliberately not distinguishable from "brand new": a Boat whose file has
// gone must look ignorant, not authoritative.
func (store *Store) ObservedEpoch() (int64, error) {
	var epoch int64
	err := store.database.View(func(transaction *bbolt.Tx) error {
		var err error
		epoch, err = observedEpoch(transaction)
		return err
	})
	if err != nil {
		return 0, err
	}
	return epoch, nil
}

func observedEpoch(transaction *bbolt.Tx) (int64, error) {
	epoch, _, err := getRecord[int64](transaction.Bucket(metaBucket), observedEpochKey)
	if err != nil {
		return 0, err
	}
	return epoch, nil
}

// bumpObservedEpoch advances the epoch inside the caller's transaction, and must
// be called by every write that changes something Snapshot reports.
//
// The read-modify-write below needs no lock of ours: bbolt admits one write
// transaction at a time, so two concurrent bumps are serialised by the file
// itself and cannot both read the same current value. That serialisation is also
// why the new epoch must never be computed in one transaction and stored in
// another — two writers would then land the same number, and a repeated epoch is
// a CAS token that matches a state its holder never read.
func bumpObservedEpoch(transaction *bbolt.Tx) (int64, error) {
	current, err := observedEpoch(transaction)
	if err != nil {
		return 0, err
	}
	next := current + 1
	if err := putRecord(transaction.Bucket(metaBucket), observedEpochKey, next); err != nil {
		return 0, err
	}
	return next, nil
}
