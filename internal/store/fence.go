package store

import (
	"errors"
	"fmt"

	"go.etcd.io/bbolt"
)

// ErrFenceRegression means an epoch was asked to move backwards.
//
// An epoch that can go backwards is not a fence. The epoch is what stops a VM
// being booted on its old host after a migration has moved it: Atlas raises the
// epoch on the losing host precisely so that a late start there is refused, and
// accepting a lower one afterwards re-opens exactly the window the fence exists
// to close — two live copies of one VM writing to the same disks.
var ErrFenceRegression = errors.New("fence epoch may not move backwards")

// SetFenceEpoch records the epoch Atlas has issued for uuid on this host. It
// accepts any forward move and refuses every backward one with
// ErrFenceRegression, leaving the epoch already held untouched.
//
// Re-asserting the epoch already held is allowed and is a no-op. Atlas retries —
// a PUT whose reply was lost is repeated verbatim — and refusing the repeat
// would leave a VM whose first assert did land permanently unbootable, a
// self-inflicted outage caused by a dropped response. Equality is also not a
// regression in the sense that matters: the same number from the sole issuer is
// the same claim on the same host, whereas only a strictly lower number says the
// claim has since moved on and this caller is looking at the past.
//
// A re-assert does not bump the observed epoch either, because it leaves the
// export byte-for-byte what it was. An epoch that moved anyway would invalidate
// every outstanding CAS token on account of a write that changed nothing.
func (store *Store) SetFenceEpoch(uuid string, epoch int64) error {
	return store.database.Update(func(transaction *bbolt.Tx) error {
		return setFenceEpoch(transaction, uuid, epoch)
	})
}

func setFenceEpoch(transaction *bbolt.Tx, uuid string, epoch int64) error {
	bucket := transaction.Bucket(fenceBucket)
	held, found, err := getRecord[int64](bucket, uuid)
	if err != nil {
		return err
	}
	if found && epoch < held {
		return fmt.Errorf("%w: %s holds epoch %d, refusing %d", ErrFenceRegression, uuid, held, epoch)
	}
	if found && epoch == held {
		return nil
	}
	if err := putRecord(bucket, uuid, epoch); err != nil {
		return err
	}
	_, err = bumpObservedEpoch(transaction)
	return err
}

// FenceEpoch reports the epoch this host holds for uuid. found=false means it
// holds none, which is not an error and is not a zero epoch either: it is the
// input that makes fence.Allow refuse to boot the VM at all.
func (store *Store) FenceEpoch(uuid string) (int64, bool, error) {
	var epoch int64
	var found bool
	err := store.database.View(func(transaction *bbolt.Tx) error {
		var err error
		epoch, found, err = getRecord[int64](transaction.Bucket(fenceBucket), uuid)
		return err
	})
	if err != nil {
		return 0, false, err
	}
	return epoch, found, nil
}

// FenceEpochs returns every epoch this host holds, keyed by UUID. The map is
// empty rather than nil on a host that holds none, because "this host fences
// nothing" is a fact Atlas needs stated rather than a null to interpret.
func (store *Store) FenceEpochs() (map[string]int64, error) {
	var epochs map[string]int64
	err := store.database.View(func(transaction *bbolt.Tx) error {
		var err error
		epochs, err = fenceEpochs(transaction)
		return err
	})
	if err != nil {
		return nil, err
	}
	return epochs, nil
}

// fenceEpochs takes the caller's transaction so that Snapshot can read it
// alongside the observed epoch that describes it.
func fenceEpochs(transaction *bbolt.Tx) (map[string]int64, error) {
	epochs := map[string]int64{}
	err := transaction.Bucket(fenceBucket).ForEach(func(key, value []byte) error {
		epoch, err := decodeRecord[int64](key, value)
		if err != nil {
			return err
		}
		epochs[string(key)] = epoch
		return nil
	})
	if err != nil {
		return nil, err
	}
	return epochs, nil
}
