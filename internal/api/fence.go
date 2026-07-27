package api

import (
	"errors"
	"fmt"

	"github.com/frappe/boat/internal/fence"
)

// refuseUnfenced is the boot gate. It answers nil when this host may start the
// VM, and the refusal to return to the caller when it may not.
//
// It runs before the operation is claimed, not inside the claim. A start refused
// for want of a fence has to stay retryable under the same Atlas Task name,
// because the retry that matters is the one right after Atlas asserts the epoch;
// an identifier already burnt on a refusal would replay that refusal forever.
//
// Stop is deliberately not gated. A host may always stop what it is running, and
// a stop refused for want of a fence is how two live copies of one VM stay alive.
func (server *Server) refuseUnfenced(uuid string) *errorResponse {
	err := server.allowedToBoot(uuid)
	switch {
	case errors.Is(err, fence.ErrNoFence):
		return conflict("This host holds no fence epoch for " + uuid + " and will not boot it until Atlas asserts one.")
	case errors.Is(err, fence.ErrStaleEpoch):
		return conflict("The fence epoch this host holds for " + uuid + " has been superseded, so it will not boot it.")
	case err != nil:
		return internalFault("The fence could not be consulted.", err)
	}
	return nil
}

// allowedToBoot asks the fence what this host may do with uuid.
//
// A start request carries no epoch of its own — it is Atlas saying "run what you
// were told to run" — so the epoch asked for is the one Atlas last asserted in
// the desired record, and a VM with no desired record asks only for the fence
// this host already holds. Either way the refusal that matters is the empty one:
// a Boat that lost its store boots nothing until Atlas re-asserts, because the
// VM whose artifacts are still on this disk may already be running on the host it
// was migrated to.
func (server *Server) allowedToBoot(uuid string) error {
	heldEpoch, held, err := server.state.FenceEpoch(uuid)
	if err != nil {
		return fmt.Errorf("could not read the fence epoch for %s: %w", uuid, err)
	}
	record, found, err := server.state.GetDesired(uuid)
	if err != nil {
		return fmt.Errorf("could not read the desired state for %s: %w", uuid, err)
	}
	requestedEpoch := heldEpoch
	if found {
		requestedEpoch = record.BootEpoch
	}
	return fence.Allow(heldEpoch, held, requestedEpoch)
}
