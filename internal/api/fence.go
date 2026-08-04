package api

import (
	"errors"
	"fmt"
	"github.com/frappe/boat/internal/wire"

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
	case errors.Is(err, fence.ErrNoAuthority):
		// A separate sentence from the no-fence one on purpose. No fence sends an
		// operator to re-register a host that lost its store; no authority tells
		// them this host has been told to stop holding intent for a VM another host
		// may now own, and that re-asserting it here is the thing not to do
		// reflexively.
		return conflictBecause(wire.ErrorReasonNoFence,
			"This host holds no desired state for "+uuid+
				", so it will not boot it; assert the desired state first.")
	case errors.Is(err, fence.ErrStaleEpoch):
		return conflict("The fence epoch this host holds for " + uuid + " has been superseded, so it will not boot it.")
	case errors.Is(err, fence.ErrWrongServer):
		return conflict("This host is not where " + uuid + " is placed, so it will not boot it.")
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
// Closing it needs two things. One now exists: desired state can be RETRACTED,
// so a host that no longer owns a VM can be told to stop holding intent for it
// (DELETE /vms/{uuid}, and terminate does it for itself).
//
// Retraction arrived with this gate letting a retracted VM boot, which is the
// opposite of the point, and it is worth writing down because the reasoning
// looked sound. The retraction keeps the epoch — a retraction removes an
// authority and must not grant one, since clearing the fence leaves the host
// holding NO epoch, which any fresh PUT satisfies including a stale one from a
// partitioned Atlas. That argument is right. What it missed is that the desired
// record is the only carrier of a REQUESTED epoch, so deleting it made this
// function compare the held epoch with itself and return nil — permanently, and
// for the one host that most needs refusing: an evacuated migration source,
// whose tree survives a keep-address repoint, so the Exists gate passes too.
// Before retraction existed Atlas's tool was PUT desired_power=Stopped, which
// start and wake did refuse. So the feature removed a working guard.
//
// Hence: NO DESIRED RECORD IS A REFUSAL. It is the same rule the reconciler has
// always applied — reconcile/converge.go treats an absent record as no authority
// to act — and the verbs simply did not share it. A VM this host holds no intent
// for is not a VM this host may boot, whatever epoch it still remembers.
//
// The other does not: Atlas must BUMP the epoch at a migration's repoint, and
// nothing in Atlas writes boot_epoch except the initial 1. Until it does, every
// epoch this host ever sees for a UUID is the same number, and the comparison
// above has nothing to refuse. Split-brain is meanwhile prevented by phase
// ordering and desired_power — which spec/33 §9 says explicitly is NOT what
// should prevent it.
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
	if !found {
		return fmt.Errorf(
			"%w: this host holds no desired state for %s, so it holds no authority to boot it",
			fence.ErrNoAuthority, uuid,
		)
	}
	// Placement gates the boot alongside the epoch (§11.1): a VM Atlas has assigned
	// to another host is refused here even at a valid epoch. Guarded on this host
	// knowing its own name, so it is inert until bootstrap provisions --server-name.
	if err := fence.Placed(server.serverName, record.Server); err != nil {
		return err
	}
	return fence.Allow(heldEpoch, held, record.BootEpoch)
}
