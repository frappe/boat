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
// READ THIS BEFORE TRUSTING THE FENCE. Today it enforces exactly one rule: a
// host that holds no epoch for a UUID will not boot it. That rule is real and it
// is the important one for a Boat that lost its store — the VM whose artifacts
// are still on this disk may already be running on the host it was migrated to,
// so it boots nothing until Atlas re-asserts.
//
// The epoch COMPARISON, however, is currently a tautology and cannot refuse
// anything. PUT writes the fence and the desired record from one document
// (internal/api/desired.go), so heldEpoch and record.BootEpoch are equal by
// construction on every path that could reach here, and the no-desired-record
// case compares the held epoch with itself. fence.ErrStaleEpoch is therefore
// unreachable from this function.
//
// Closing it needs two things that do not exist yet, both on the Atlas side:
// Atlas must BUMP the epoch at a migration's repoint (nothing in Atlas writes
// boot_epoch except the initial 1), and it must be able to RETRACT or supersede
// desired state on the host that no longer owns the VM (there is no DELETE, and
// the source keeps its stale record forever). Until both land, split-brain is
// prevented by phase ordering and desired_power — which spec/33 §9 says
// explicitly is NOT what should prevent it.
//
// Do not delete this comment when the comparison starts working; delete it when
// a test proves a stale epoch is refused.
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
