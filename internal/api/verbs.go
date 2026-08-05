package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/netapply/reservedip"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/vm"
	"github.com/frappe/boat/internal/wire"
)

// The verbs are named in the host CLI's grammar, so an operation record reads
// the same after the port as the Task row it replaces did before it — and so
// that Atlas's own OPERATION_VERBS table (atlas/atlas/boat_client.py) and this
// one are the same nine strings rather than two spellings of them.
const (
	verbStartVirtualMachine     = "start-vm"
	verbStopVirtualMachine      = "stop-vm"
	verbPauseVirtualMachine     = "pause-vm"
	verbResumeVirtualMachine    = "resume-vm"
	verbSleepVirtualMachine     = "sleep-vm"
	verbWakeVirtualMachine      = "wake-vm"
	verbRebuildVirtualMachine   = "rebuild-vm"
	verbTerminateVirtualMachine = "terminate-vm"
	verbResizeVirtualMachine    = "resize-vm"
	// The reserved-IP verb keeps the Python script's own name (vm-reserved-ip.py),
	// not the <verb>-vm shape of the nine lifecycle verbs, so an operation record
	// and Atlas's boat_client verb table read the same string.
	verbReservedIPVirtualMachine = "vm-reserved-ip"
)

func (server *Server) PauseVirtualMachine(ctx context.Context, request wire.PauseVirtualMachineRequestObject) (wire.PauseVirtualMachineResponseObject, error) {
	operation, failure := server.operation(ctx, request.Body, verbPauseVirtualMachine, request.Uuid,
		func(ctx context.Context, runner *run.Runner) (model.OperationResult, error) {
			return nil, server.virtualMachines.Pause(ctx, runner, request.Uuid)
		})
	if failure != nil {
		return failure, nil
	}
	return wire.PauseVirtualMachine200JSONResponse{
		OperationAcceptedJSONResponse: wire.OperationAcceptedJSONResponse(operationToWire(operation)),
	}, nil
}

// ResumeVirtualMachine unfreezes a guest, and is subject to §11.3 even though it
// boots nothing: a VM whose desired power is Stopped is one an operator took
// down, and handing its vCPUs back would undo that as surely as a start would.
//
// It is deliberately NOT fenced. The fence is the boot gate, and a resume acts
// on a Firecracker that is already resident on this host — refusing it would
// leave a live guest frozen with no way to thaw it, which is the same reason
// stop is not gated either.
func (server *Server) ResumeVirtualMachine(ctx context.Context, request wire.ResumeVirtualMachineRequestObject) (wire.ResumeVirtualMachineResponseObject, error) {
	if failure := server.refuseStoppedDesire(request.Uuid, "resume"); failure != nil {
		return failure, nil
	}
	operation, failure := server.operation(ctx, request.Body, verbResumeVirtualMachine, request.Uuid,
		func(ctx context.Context, runner *run.Runner) (model.OperationResult, error) {
			return nil, server.virtualMachines.Resume(ctx, runner, request.Uuid)
		})
	if failure != nil {
		return failure, nil
	}
	return wire.ResumeVirtualMachine200JSONResponse{
		OperationAcceptedJSONResponse: wire.OperationAcceptedJSONResponse(operationToWire(operation)),
	}, nil
}

// SleepVirtualMachine parks a VM to free its RAM.
//
// The uid the snapshot directory is chowned to is read off the host inside the
// verb rather than carried on the wire: it is the host's own record of the jail
// it built, and a caller's copy of it can be stale in a way that fails silently.
// See vm.Manager.FirecrackerUID.
func (server *Server) SleepVirtualMachine(ctx context.Context, request wire.SleepVirtualMachineRequestObject) (wire.SleepVirtualMachineResponseObject, error) {
	operation, failure := server.operation(ctx, request.Body, verbSleepVirtualMachine, request.Uuid,
		func(ctx context.Context, runner *run.Runner) (model.OperationResult, error) {
			return server.sleep(ctx, runner, request.Uuid)
		})
	if failure != nil {
		return failure, nil
	}
	return wire.SleepVirtualMachine200JSONResponse{
		OperationAcceptedJSONResponse: wire.OperationAcceptedJSONResponse(operationToWire(operation)),
	}, nil
}

// sleep takes the memory snapshot if it can, parks the VM either way, and states
// which of the two happened.
//
// It is the one verb of the nine with a typed result, and that result is not a
// nicety. Whether the guest's RAM was captured is decided ON THE HOST — an old
// launcher, a full filesystem or a socket that will not answer each fall back to
// a plain stop — and it is stated nowhere else, so Atlas books
// `has_memory_snapshot` from it. Before the contract had a field to carry it,
// Atlas's sleep raised AFTER the verb had already succeeded: the VM parked, the
// Task committed Success, the row stayed Running, and the idle sweeper re-slept
// the same VM once a minute forever.
func (server *Server) sleep(
	ctx context.Context, runner *run.Runner, uuid string,
) (model.OperationResult, error) {
	firecrackerUID, err := server.virtualMachines.FirecrackerUID(ctx, runner, uuid)
	if err != nil {
		return nil, err
	}
	result, err := server.virtualMachines.Sleep(ctx, runner, uuid, vm.SleepRequest{FirecrackerUID: firecrackerUID})
	if err != nil {
		return nil, err
	}
	if result.Reason != "" {
		slog.Warn("a virtual machine slept without a memory snapshot", "uuid", uuid, "reason", result.Reason)
	}
	return sleepResult(result), nil
}

// sleepResult is what this verb's one `ATLAS_RESULT=` line has always carried,
// so a Task row reads the same whichever transport filled it — the three fields
// of scripts/sleep-vm.py's SleepVmResult, and of the Fake provider that stands in
// for it (atlas/atlas/providers/fake_tasks.py).
//
// `memory_snapshot` is always stated, because it is the answer and an absent key
// must never be read as a false one. The other two are stated only when they say
// something: an empty reason is not a reason and a zero size is not a
// measurement, and a key that is present and empty invites a reader to show it.
//
// The reason travels rather than staying in the daemon's log because it is the
// sole record of WHY this VM will cold-boot on its next wake, and it names a host
// fault an operator can fix — a launcher that predates memory snapshots wants a
// re-provision, a full filesystem wants space. The log is on the host; the
// operator is in Atlas, reading the Task.
func sleepResult(result vm.SleepResult) model.OperationResult {
	values := model.OperationResult{"memory_snapshot": result.MemorySnapshot}
	if result.Reason != "" {
		values["reason"] = result.Reason
	}
	if result.MemorySnapshotBytes != 0 {
		values["memory_snapshot_bytes"] = result.MemorySnapshotBytes
	}
	return values
}

// WakeVirtualMachine resumes a sleeping VM now, instead of waiting for the SYN
// that would have done it.
//
// Both gates before the claim, and both for the reason the fence gate on start
// gives: a refusal that becomes possible the moment Atlas asserts something must
// not burn the operation identifier, or the retry after the assertion would
// replay the refusal forever.
func (server *Server) WakeVirtualMachine(ctx context.Context, request wire.WakeVirtualMachineRequestObject) (wire.WakeVirtualMachineResponseObject, error) {
	if failure := server.refuseStoppedDesire(request.Uuid, "wake"); failure != nil {
		return failure, nil
	}
	// Waking is booting — the marker comes off and the unit comes up — so it is
	// fenced exactly as a cold start is, and exactly as reconcile.wake is.
	if failure := server.refuseUnfenced(request.Uuid); failure != nil {
		return failure, nil
	}
	operation, failure := server.operation(ctx, request.Body, verbWakeVirtualMachine, request.Uuid,
		func(ctx context.Context, runner *run.Runner) (model.OperationResult, error) {
			return nil, server.virtualMachines.Wake(ctx, runner, request.Uuid)
		})
	if failure != nil {
		return failure, nil
	}
	return wire.WakeVirtualMachine200JSONResponse{
		OperationAcceptedJSONResponse: wire.OperationAcceptedJSONResponse(operationToWire(operation)),
	}, nil
}

// TerminateVirtualMachine deletes everything this host holds for a VM, and
// retracts the desired state that would otherwise bring it back.
//
// The retraction is part of the verb rather than a second call Atlas is trusted
// to make afterwards, because the failure it prevents needs no mistake by Atlas
// at all: a terminate leaves the VM Stopped, the sweep sees Stopped against a
// desired_power of Running, passes the fence — which is still held — and runs
// `systemctl start` on a VM whose root volume was just lvremoved. Every thirty
// seconds, forever. A verb that destroys a VM and leaves the assertion that
// resurrects it has not finished.
//
// It runs BEFORE the mechanics, and that order is deliberate. Desired state is
// stale from the instant Atlas asks for a terminate — nothing Boat does next can
// make Running true again — so the record that would fight the teardown is gone
// before the teardown starts, and a crash anywhere inside the mechanics leaves a
// half-torn-down VM that nothing tries to boot. The other order leaves precisely
// the window this verb exists to close, at the moment the VM is least able to
// survive being started.
//
// The fence epoch is deliberately kept; see retract.
func (server *Server) TerminateVirtualMachine(ctx context.Context, request wire.TerminateVirtualMachineRequestObject) (wire.TerminateVirtualMachineResponseObject, error) {
	operation, failure := server.operation(ctx, request.Body, verbTerminateVirtualMachine, request.Uuid,
		func(ctx context.Context, runner *run.Runner) (model.OperationResult, error) {
			if err := server.state.DeleteDesired(request.Uuid); err != nil {
				return nil, fmt.Errorf("the desired state of %s could not be retracted: %w", request.Uuid, err)
			}
			return nil, server.virtualMachines.Terminate(ctx, runner, request.Uuid)
		})
	if failure != nil {
		return failure, nil
	}
	return wire.TerminateVirtualMachine200JSONResponse{
		OperationAcceptedJSONResponse: wire.OperationAcceptedJSONResponse(operationToWire(operation)),
	}, nil
}

// ResizeVirtualMachine applies the shape Atlas has already asserted.
//
// The request carries no numbers at all. They are desired state, and reading
// them from the store rather than the wire is what makes the retry of a resize
// safe: two attempts apply the same shape, and the request and the store can
// never state two different ones for one VM.
func (server *Server) ResizeVirtualMachine(ctx context.Context, request wire.ResizeVirtualMachineRequestObject) (wire.ResizeVirtualMachineResponseObject, error) {
	resize, failure := server.resizeRequest(request.Uuid)
	if failure != nil {
		return failure, nil
	}
	operation, failure := server.operation(ctx, request.Body, verbResizeVirtualMachine, request.Uuid,
		func(ctx context.Context, runner *run.Runner) (model.OperationResult, error) {
			return nil, server.virtualMachines.Resize(ctx, runner, request.Uuid, resize)
		})
	if failure != nil {
		return failure, nil
	}
	return wire.ResizeVirtualMachine200JSONResponse{
		OperationAcceptedJSONResponse: wire.OperationAcceptedJSONResponse(operationToWire(operation)),
	}, nil
}

// RebuildVirtualMachine lays a VM's root filesystem down again.
//
// The only verb whose request carries more than an identifier, because the
// source and the guest identity are neither desired state nor host mechanics:
// which image to reinstall from is a choice made at the moment of asking, and
// the identity is what the fresh filesystem has to be told about itself. The
// sizes it grows to ARE desired state and are read from the store; the per-VM
// uid is the host's own and is read from the host.
func (server *Server) RebuildVirtualMachine(ctx context.Context, request wire.RebuildVirtualMachineRequestObject) (wire.RebuildVirtualMachineResponseObject, error) {
	if request.Body == nil || request.Body.OperationId == "" {
		return missingOperationIdentifier(), nil
	}
	rebuild, failure := server.rebuildRequest(request.Uuid, *request.Body)
	if failure != nil {
		return failure, nil
	}
	build := func(ctx context.Context, runner *run.Runner) (model.OperationResult, error) {
		if err := server.decisions.Record(rebuildSource(request.Body.OperationId, rebuild)); err != nil {
			return nil, err
		}
		firecrackerUID, err := server.virtualMachines.FirecrackerUID(ctx, runner, request.Uuid)
		if err != nil {
			return nil, err
		}
		rebuild.FirecrackerUID = firecrackerUID
		return nil, server.virtualMachines.Rebuild(ctx, runner, request.Uuid, rebuild)
	}
	operation, failure := server.perform(ctx, request.Body.OperationId, verbRebuildVirtualMachine, request.Uuid, build)
	if failure != nil {
		return failure, nil
	}
	return wire.RebuildVirtualMachine200JSONResponse{
		OperationAcceptedJSONResponse: wire.OperationAcceptedJSONResponse(operationToWire(operation)),
	}, nil
}

// rebuildSource is the write-ahead decision of the one verb among the nine that
// makes a choice its own retry could not repeat (spec/33-boat.md §11.5). It is
// recorded before the rebuild runs, because the rebuild's first act is to drop
// the VM's root volume — after that the question "what was this supposed to
// become" has no answer on the host at all.
//
// What makes it a decision rather than a log line: the source is the only verb
// input that is neither desired state nor host state. It is chosen at the moment
// of asking and written down nowhere else, so a crash between the claim and the
// terminal record loses it — the operation record remembers the verb and the
// UUID, and only this remembers which filesystem the VM was authorized to be
// rebuilt from.
//
// The request's sources are recorded as stated, not the single origin
// vm.Rebuild resolves them to. The precedence rule — a snapshot device wins over
// an image — belongs to internal/vm, and restating it here would let this record
// start lying the day that rule changes.
func rebuildSource(operationID string, request vm.RebuildRequest) model.Decision {
	return model.Decision{
		OperationID: operationID,
		Step:        "rebuild-source",
		Values: map[string]string{
			"snapshot_device":      request.SnapshotDevice,
			"image":                request.Image,
			"data_snapshot_device": request.DataSnapshotDevice,
		},
	}
}

// ReservedIpVirtualMachine attaches or detaches a Reserved IP's host-side 1:1 NAT.
//
// The reserved IP is the one input a caller states — it is the public identity
// Atlas allocated, neither host state nor desired power — so it is validated here,
// at the boundary, as an actual IPv4 before it is carried any further: a value
// that is not an address has no business reaching an nft rule, and refusing it as a
// 400 keeps it out of the operation journal too. The guest and veth the NAT is
// built around are NOT taken from the caller; the verb reads them off the host.
//
// It is not fenced: attaching NAT to an already-running proxy VM boots nothing, so
// the boot gate does not apply, exactly as it does not to pause or resume.
func (server *Server) ReservedIpVirtualMachine(ctx context.Context, request wire.ReservedIpVirtualMachineRequestObject) (wire.ReservedIpVirtualMachineResponseObject, error) {
	if request.Body == nil || request.Body.OperationId == "" {
		return missingOperationIdentifier(), nil
	}
	reserved, failure := reservedIPRequest(*request.Body)
	if failure != nil {
		return failure, nil
	}
	operation, failure := server.perform(ctx, request.Body.OperationId, verbReservedIPVirtualMachine, request.Uuid,
		func(ctx context.Context, runner *run.Runner) (model.OperationResult, error) {
			delivery, err := server.virtualMachines.ReservedIP(ctx, runner, request.Uuid, reserved)
			if err != nil {
				return nil, err
			}
			return reservedIPResult(reserved, delivery), nil
		})
	if failure != nil {
		return failure, nil
	}
	return wire.ReservedIpVirtualMachine200JSONResponse{
		OperationAcceptedJSONResponse: wire.OperationAcceptedJSONResponse(operationToWire(operation)),
	}, nil
}

// reservedIPRequest turns the wire body into the verb's request, refusing an
// action that is neither attach nor detach and an attach whose reserved IP is
// missing or not an address. The address is canonicalised here so the value that
// reaches network.env and the ruleset is the one net/netip vouches for.
func reservedIPRequest(body wire.ReservedIpRequest) (vm.ReservedIPRequest, *errorResponse) {
	switch body.Action {
	case wire.ReservedIpRequestActionDetach:
		return vm.ReservedIPRequest{Detach: true}, nil
	case wire.ReservedIpRequestActionAttach:
		if body.ReservedIpv4 == nil || *body.ReservedIpv4 == "" {
			return vm.ReservedIPRequest{}, badRequest("a reserved-ip attach needs reserved_ipv4.")
		}
		address, err := netip.ParseAddr(*body.ReservedIpv4)
		if err != nil || !address.Is4() {
			return vm.ReservedIPRequest{}, badRequest("reserved_ipv4 " + *body.ReservedIpv4 + " is not an IPv4 address.")
		}
		return vm.ReservedIPRequest{ReservedIPv4: address.String()}, nil
	default:
		return vm.ReservedIPRequest{}, badRequest("action must be attach or detach, got " + string(body.Action) + ".")
	}
}

// reservedIPResult states which delivery model the host used, so Atlas's Task
// shows whether the reserved IP came in over a DigitalOcean anchor or was routed
// straight to the host. A detach has nothing to report — it removed a NAT and the
// verb succeeded, and an absent result is not a false one.
func reservedIPResult(request vm.ReservedIPRequest, delivery reservedip.Delivery) model.OperationResult {
	if request.Detach {
		return nil
	}
	if delivery.Anchored {
		return model.OperationResult{"delivery": "anchor", "anchor_address": delivery.Anchor.Address}
	}
	return model.OperationResult{"delivery": "routed"}
}

// operation is the shared front of every verb whose whole request is the
// operation identifier: refuse what could not be replayed, then take it through
// perform like everything else.
func (server *Server) operation(
	ctx context.Context, body *wire.OperationRequest, verb string, uuid string, execute hostWork,
) (model.Operation, *errorResponse) {
	if body == nil || body.OperationId == "" {
		return model.Operation{}, missingOperationIdentifier()
	}
	return server.perform(ctx, body.OperationId, verb, uuid, execute)
}
