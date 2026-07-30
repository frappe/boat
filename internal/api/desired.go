package api

import (
	"context"
	"errors"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/store"
	"github.com/frappe/boat/internal/wire"
)

// PutVirtualMachine records what Atlas wants of one VM, and the fence epoch that
// permits this host to boot it at all.
//
// It records intent and does nothing else. WO-1 has no reconciler, so a PUT that
// flips desired_power neither starts nor stops anything — the loop that acts on
// this arrives in WO-2. Do not "helpfully" start a VM from here: work run inside
// a PUT has no operation record behind it, cannot be replayed under an Atlas Task
// name, and would race the reconciler that is about to own the decision.
//
// It is idempotent by construction, because a PUT is how intent is re-asserted
// after a partition: the same document arriving twice writes nothing.
//
// It is also where §11.2's compare-and-set lands, because this is the operation
// the rule names: a caller that took a contended decision from the mirror sends
// the observed epoch it decided from as If-Match and is refused if this host has
// moved underneath it. The precondition is checked after the document is
// understood and before anything is written — a malformed body is worth saying
// so about whether or not the caller is also stale.
func (server *Server) PutVirtualMachine(ctx context.Context, request wire.PutVirtualMachineRequestObject) (wire.PutVirtualMachineResponseObject, error) {
	if failure := server.refuseMalformedUUID(request.Uuid); failure != nil {
		return failure, nil
	}
	if request.Body == nil {
		return badRequest("This request needs a desired state document."), nil
	}
	asserted, err := desiredFromWire(request.Uuid, *request.Body)
	if err != nil {
		return badRequest(err.Error()), nil
	}
	if failure := server.refuseMovedObservation(request.Uuid, request.Params.IfMatch); failure != nil {
		return failure, nil
	}
	stored, failure := server.assert(asserted)
	if failure != nil {
		return failure, nil
	}
	return wire.PutVirtualMachine200JSONResponse(desiredToWire(stored)), nil
}

// DeleteVirtualMachine retracts what Atlas wanted of one VM. It is the PUT's
// mirror, and it is the only way an assertion is ever taken back.
//
// Nothing on the host is touched. A running VM goes on running; what ends is
// this host's authority to act on it, because the reconciler reads a missing
// desired record as "no assertion from Atlas is no authority to act". That is
// what a migration's source host needs once the VM has been repointed away from
// it, and it is what spec §16.0 names as one of the two things missing before
// the fence means anything.
//
// The fence epoch stays. See retract for why that is the whole design and not a
// loose end.
//
// Idempotent, and it has to be: a retraction whose reply was lost is repeated,
// and answering 404 to the repeat would tell Atlas the host still holds intent
// it does not hold.
func (server *Server) DeleteVirtualMachine(ctx context.Context, request wire.DeleteVirtualMachineRequestObject) (wire.DeleteVirtualMachineResponseObject, error) {
	if failure := server.refuseMalformedUUID(request.Uuid); failure != nil {
		return failure, nil
	}
	if failure := server.retract(request.Uuid); failure != nil {
		return failure, nil
	}
	return wire.DeleteVirtualMachine204Response{}, nil
}

// retract drops the desired record and keeps the fence epoch.
//
// Both halves are load-bearing. Dropping the record is what stops the sweep:
// the reconciler acts only on VMs it holds an assertion for, so a VM with none
// is left exactly as the host has it — which for a terminated VM means nothing
// tries to start a guest whose root volume was just lvremoved, forever.
//
// Keeping the epoch is the half that looks like an oversight and is not. The
// epoch says which incarnation of this UUID this host may boot; clearing it
// would make the VM unbootable here, which is right for a VM that is gone and
// dangerous for one that is not, because "no epoch held" is the state a fresh
// PUT of ANY epoch — including a stale one — satisfies. A host that retracted
// and then met a partitioned Atlas would accept an epoch it had already seen
// superseded and boot a VM that has since moved. The epoch is issued by Atlas
// and only ever moves forward (§11.1), so leaving it costs a few bytes per UUID
// this host has ever heard of and buys the one guarantee retraction must not
// give away.
//
// Re-provisioning the same UUID here therefore still works: the PUT that
// precedes it re-asserts the desired record against an epoch that is equal to or
// newer than the tombstone, which the store accepts.
func (server *Server) retract(uuid string) *errorResponse {
	if err := server.state.DeleteDesired(uuid); err != nil {
		return internalFault("The desired state could not be retracted.", err)
	}
	return nil
}

// assert writes the fence before the intent it authorizes. The other order would
// leave a desired state recorded at an epoch this host was refused — intent it
// must never act on, sitting in the store looking exactly like intent it should.
func (server *Server) assert(asserted model.DesiredVirtualMachine) (model.DesiredVirtualMachine, *errorResponse) {
	stored, unchanged, failure := server.alreadyAsserted(asserted)
	if failure != nil || unchanged {
		return stored, failure
	}
	if err := server.state.SetFenceEpoch(asserted.UUID, asserted.BootEpoch); err != nil {
		return asserted, refusedEpoch(asserted.UUID, err)
	}
	if err := server.state.PutDesired(asserted); err != nil {
		return asserted, internalFault("The desired state could not be recorded.", err)
	}
	return asserted, nil
}

// alreadyAsserted reports a re-assertion with nothing to do, and returns what is
// already stored so the caller answers with the record rather than the request.
//
// The fence is part of the comparison and not an afterthought: a host that kept
// the desired record but lost the epoch has something to do, and treating that
// as a no-op would leave a VM that Atlas believes is fenced unable to boot.
func (server *Server) alreadyAsserted(asserted model.DesiredVirtualMachine) (model.DesiredVirtualMachine, bool, *errorResponse) {
	stored, found, err := server.state.GetDesired(asserted.UUID)
	if err != nil {
		return asserted, false, internalFault("The desired state could not be read.", err)
	}
	heldEpoch, held, err := server.state.FenceEpoch(asserted.UUID)
	if err != nil {
		return asserted, false, internalFault("The fence epoch could not be read.", err)
	}
	unchanged := found && held && heldEpoch == asserted.BootEpoch && sameDesire(stored, asserted)
	return stored, unchanged, nil
}

// sameDesire compares two assertions by what they ask for. AssertedAt is when
// Atlas last said it, which differs on every re-assert and is exactly what a
// re-assert must not count as a change.
func sameDesire(stored, asserted model.DesiredVirtualMachine) bool {
	stored.AssertedAt = asserted.AssertedAt
	return stored == asserted
}

// refusedEpoch names the one refusal the caller can act on. An epoch that went
// backwards is Atlas asserting a claim this host has already seen superseded —
// answering 200 to it would tell the loser of a migration that it won.
func refusedEpoch(uuid string, err error) *errorResponse {
	if errors.Is(err, store.ErrFenceRegression) {
		// With its reason, because this 409 and the stale-observation one above it
		// mean opposite things to a caller: this one must not be retried, since
		// retrying it re-asserts a claim this host has already seen superseded.
		return conflictBecause(wire.ErrorReasonFenceRegression,
			"The fence epoch offered for "+uuid+" is older than the one this host already holds.")
	}
	return internalFault("The fence epoch could not be recorded.", err)
}
