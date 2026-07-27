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
func (server *Server) PutVirtualMachine(ctx context.Context, request wire.PutVirtualMachineRequestObject) (wire.PutVirtualMachineResponseObject, error) {
	if request.Body == nil {
		return badRequest("This request needs a desired state document."), nil
	}
	asserted, err := desiredFromWire(request.Uuid, *request.Body)
	if err != nil {
		return badRequest(err.Error()), nil
	}
	stored, failure := server.assert(asserted)
	if failure != nil {
		return failure, nil
	}
	return wire.PutVirtualMachine200JSONResponse(desiredToWire(stored)), nil
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
		return conflict("The fence epoch offered for " + uuid + " is older than the one this host already holds.")
	}
	return internalFault("The fence epoch could not be recorded.", err)
}
