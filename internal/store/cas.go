package store

import (
	"errors"
	"fmt"

	"github.com/frappe/boat/internal/model"
	"go.etcd.io/bbolt"
)

// ErrObservationMoved means a caller offered a precondition that no longer
// holds: it decided from a snapshot of this host, and the part of the host it
// decided about has been written since (spec/33-boat.md §11.2).
//
// It is the store's half of the CAS. The API turns it into a 409 carrying a
// reason the caller can branch on, because "re-read the export and decide
// again" and "stop, you have lost" are opposite instructions and they share a
// status code.
var ErrObservationMoved = errors.New("this host's observation moved after the epoch offered")

// CheckVirtualMachineUnmoved is the precondition behind `If-Match` on a write
// about one VM.
//
// # The token is whole-host and the comparison is per-resource
//
// There is one monotonic observed epoch for the host, it is what `GET /export`
// carries, and it is the only number Atlas has to quote. What is scoped is the
// COMPARISON: this asks whether the record for THIS UUID has been written since
// the epoch offered, not whether anything at all on the host has.
//
// The difference is between a mechanism that works and one that is switched off
// within a week. Every observation bumps the whole-host epoch, and observations
// are the daemon's steady state: the reconciler sweeps every desired record
// every 30 seconds and writes what it saw, so a host running forty VMs bumps the
// epoch about forty times a sweep. Atlas's mirror is refreshed by a five-minute
// export sweep. A whole-host comparison would therefore be matched against an
// epoch some four hundred bumps old and would refuse literally every write. The
// failure direction is safe — it refuses rather than admits — which is exactly
// what makes it dangerous: a precondition that always fails is a precondition
// somebody removes, and removing it takes the real protection with it.
//
// # What the per-resource scope costs
//
// It protects a decision about ONE VM, and nothing else. A contended decision
// about a HOST-WIDE resource — the last slot of RAM, the thin pool's free bytes,
// the reserved-IP slot — is not covered by this comparison and must not be
// wedged into it by widening the scope back to the host, which would reintroduce
// exactly the noise above. It is covered by giving that resource a record with a
// stamp of its own and comparing against that, which is the shape WO-3b's
// reserved-IP attach and WO-4's migration-target choice should follow. Nothing
// regresses meanwhile: no host-wide contended verb exists yet.
//
// # Two ways to fail
//
// Moved: the VM's record was written at an epoch later than the one offered, so
// the caller decided from a state this host has left.
//
// Unissued: the epoch offered is newer than any this host has reached. That is a
// caller quoting a snapshot of a DIFFERENT store — a Boat whose bbolt file was
// lost and whose epoch restarted from zero, which is the state in which the
// mirror is most confidently wrong. Accepting it because "nothing has moved
// since epoch 900 on a host at epoch 3" is true would be the one reading that
// gets a VM booted twice.
//
// A UUID this host has never observed passes. Nothing about it has moved,
// because there is nothing.
func (store *Store) CheckVirtualMachineUnmoved(uuid string, offered int64) error {
	return store.database.View(func(transaction *bbolt.Tx) error {
		reached, err := observedEpoch(transaction)
		if err != nil {
			return err
		}
		record, found, err := getRecord[model.VirtualMachine](transaction.Bucket(virtualMachinesBucket), uuid)
		if err != nil {
			return err
		}
		var writtenAt int64
		if found {
			writtenAt = record.ObservedEpoch
		}
		return unmoved(offered, writtenAt, reached)
	})
}

// unmoved is the comparison itself, kept apart from the transaction so the rule
// can be stated once and read without a store under it.
//
// Both reads happen in the caller's single View, so the epoch this host has
// reached and the stamp on the record cannot come from two different instants of
// the file — which is the same reason Snapshot takes both in one transaction.
func unmoved(offered, writtenAt, reached int64) error {
	if offered > reached {
		return fmt.Errorf(
			"%w: the epoch offered is %d and this host has only ever reached %d, so it was read from another store",
			ErrObservationMoved, offered, reached,
		)
	}
	if writtenAt > offered {
		return fmt.Errorf(
			"%w: it was last written at epoch %d and the epoch offered is %d",
			ErrObservationMoved, writtenAt, offered,
		)
	}
	return nil
}
