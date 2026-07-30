package store

import (
	"github.com/frappe/boat/internal/model"
	"go.etcd.io/bbolt"
)

// PutDesired records what Atlas wants of one VM, replacing any earlier
// assertion. Desired state is latest-wins by definition: Atlas is its sole
// author, so the newest assertion is the only one that means anything and there
// is nothing here to reconcile between two versions.
//
// This does not bump the observed epoch, and that is deliberate. The epoch is a
// CAS token over what this host has *seen*; no part of a desired record appears
// in a Snapshot. Bumping here would invalidate every outstanding token each time
// Atlas re-asserted state the host had not acted on yet, which is a CAS failure
// that means nothing changed.
func (store *Store) PutDesired(record model.DesiredVirtualMachine) error {
	return store.database.Update(func(transaction *bbolt.Tx) error {
		return putRecord(transaction.Bucket(desiredBucket), record.UUID, record)
	})
}

// DeleteDesired drops what Atlas wanted of one VM, and is how intent is
// RETRACTED: after it, this host holds no assertion for the UUID and the
// reconciler treats it as one it was never told about.
//
// It leaves the fence epoch alone, and that asymmetry is the point rather than
// an omission. The desired record is an authority to act; the epoch is a claim
// about which incarnation of a UUID may boot here. Dropping the epoch with the
// record would re-open the window it exists to close — an Atlas holding a stale
// view could then assert an older one against a host that had already seen it
// superseded, and boot a VM that lives somewhere else now. The tombstone the
// epoch leaves behind is a few bytes per VM this host has ever heard of, which
// is the cheapest half of this trade.
//
// Deleting what is not there is not an error: the caller's question is "hold
// nothing for this UUID", and bbolt's Delete answers it either way.
func (store *Store) DeleteDesired(uuid string) error {
	return store.database.Update(func(transaction *bbolt.Tx) error {
		return transaction.Bucket(desiredBucket).Delete([]byte(uuid))
	})
}

// GetDesired reports found=false with a nil error when Atlas has never asserted
// anything about this VM to this host. Absence is an answer, not a failure — and
// on its own it is not permission to do anything either, since a VM with no
// desired record is also a VM with no fence epoch.
func (store *Store) GetDesired(uuid string) (model.DesiredVirtualMachine, bool, error) {
	var record model.DesiredVirtualMachine
	var found bool
	err := store.database.View(func(transaction *bbolt.Tx) error {
		var err error
		record, found, err = getRecord[model.DesiredVirtualMachine](transaction.Bucket(desiredBucket), uuid)
		return err
	})
	if err != nil {
		return model.DesiredVirtualMachine{}, false, err
	}
	return record, found, nil
}

// ListDesired returns every desired record this host holds, ordered by UUID.
// This is the reconciler's input: what Atlas wants, to be diffed against what
// ListVirtualMachines says the host actually has.
func (store *Store) ListDesired() ([]model.DesiredVirtualMachine, error) {
	records := []model.DesiredVirtualMachine{}
	err := store.database.View(func(transaction *bbolt.Tx) error {
		return transaction.Bucket(desiredBucket).ForEach(func(key, value []byte) error {
			record, err := decodeRecord[model.DesiredVirtualMachine](key, value)
			if err != nil {
				return err
			}
			records = append(records, record)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}
