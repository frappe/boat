// Package fence decides whether this host may boot a VM at all.
//
// It is one pure function over an epoch this host holds and an epoch a request
// carries, because that is all the decision is allowed to be. Atlas is the sole
// issuer of epochs, the store is the only thing that remembers them, and this
// package compares them. Nothing here reads the host, and nothing here guesses.
package fence

import (
	"errors"
	"fmt"
)

// ErrNoFence means Boat holds no epoch for the VM. A Boat that lost its store
// and boots everything it finds on disk is the most dangerous state in the
// system: the same VM may already be running on the host it migrated to.
var ErrNoFence = errors.New("this host holds no fence epoch for the virtual machine")

// ErrStaleEpoch means the boot was requested under an epoch older than the one
// this host holds, so the claim on the VM has moved on since the request was
// issued.
var ErrStaleEpoch = errors.New("the requested fence epoch is older than the one this host holds")

// ErrNoAuthority means Boat holds an epoch for the VM but no desired state, so
// there is nothing this host has been asked to do with it.
//
// Distinct from ErrNoFence, and the distinction is operational: no fence is a
// host that has lost its store and needs re-registering, while no authority is a
// host that has been told — by a retraction, or by a terminate — to stop holding
// intent for a VM someone else may now own. Conflating them would send an
// operator to re-assert intent on the one host that must not have it.
var ErrNoAuthority = errors.New("this host holds no desired state for the virtual machine")

// Allow reports whether this host may boot the VM under requestedEpoch, given
// the epoch its store holds and whether it holds one at all. A nil error is
// permission; any error is a refusal, and a refused caller must not start
// Firecracker.
//
// held=false is refused with ErrNoFence, and it is the case that matters most.
// A Boat that lost its bbolt file and booted everything it found on the host's
// disks is the single most dangerous state in this system, because a VM whose
// artifacts are still here may already be running on the host it was migrated
// to — two live copies writing to what the network believes is one machine, from
// which there is no clean recovery. So an empty fence store means boot nothing:
// the daemon waits for Atlas to re-assert rather than helpfully guessing, and
// the cost of being wrong is a VM that stays down until the control plane speaks
// rather than a VM that is silently running twice.
//
// The two directions of the comparison:
//
//   - requestedEpoch < heldEpoch is refused as ErrStaleEpoch. The request was
//     issued before the claim this host currently holds, which is exactly the
//     shape of a start that outlived a migration: Atlas raised the epoch here so
//     that the losing host could no longer boot, and this is the old start
//     arriving late.
//   - requestedEpoch > heldEpoch is allowed. Only Atlas issues epochs, so a
//     number higher than any this host has seen cannot be a replay of an older
//     placement — it is a newer claim from the sole issuer and this host is
//     merely behind. Refusing it would wedge a start that raced the assert
//     authorising it, and buy no safety. This is the same rule the store applies
//     to SetFenceEpoch, which accepts every forward move and refuses only
//     backward ones.
//
// Equal epochs are allowed: a re-assert of the same claim is the same claim, and
// the ordinary steady state is a start requested at exactly the epoch held.
func Allow(heldEpoch int64, held bool, requestedEpoch int64) error {
	if !held {
		return ErrNoFence
	}
	if requestedEpoch < heldEpoch {
		return fmt.Errorf("%w: requested %d, this host holds %d", ErrStaleEpoch, requestedEpoch, heldEpoch)
	}
	return nil
}
