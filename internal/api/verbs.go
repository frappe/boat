package api

import (
	"context"
	"log/slog"

	"github.com/frappe/boat/internal/model"
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
)

func (server *Server) PauseVirtualMachine(ctx context.Context, request wire.PauseVirtualMachineRequestObject) (wire.PauseVirtualMachineResponseObject, error) {
	operation, failure := server.operation(ctx, request.Body, verbPauseVirtualMachine, request.Uuid,
		func(runner *run.Runner) error {
			return server.virtualMachines.Pause(ctx, runner, request.Uuid)
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
		func(runner *run.Runner) error {
			return server.virtualMachines.Resume(ctx, runner, request.Uuid)
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
		func(runner *run.Runner) error { return server.sleep(ctx, runner, request.Uuid) })
	if failure != nil {
		return failure, nil
	}
	return wire.SleepVirtualMachine200JSONResponse{
		OperationAcceptedJSONResponse: wire.OperationAcceptedJSONResponse(operationToWire(operation)),
	}, nil
}

// sleep takes the memory snapshot if it can and parks the VM either way.
//
// The reason a snapshot was not taken is logged rather than returned, because
// the sleep succeeded: the caller asked for the VM's RAM back and got it. What
// the reason costs is the next wake's speed, and the fact that it will be a cold
// boot is already observable — it is `has_memory_snapshot` on the record this
// verb writes afterwards. The sentence explaining WHY exists only here, and an
// operator reading the daemon's log is the one who can act on it.
func (server *Server) sleep(ctx context.Context, runner *run.Runner, uuid string) error {
	firecrackerUID, err := server.virtualMachines.FirecrackerUID(ctx, runner, uuid)
	if err != nil {
		return err
	}
	result, err := server.virtualMachines.Sleep(ctx, runner, uuid, vm.SleepRequest{FirecrackerUID: firecrackerUID})
	if err == nil && result.Reason != "" {
		slog.Warn("a virtual machine slept without a memory snapshot", "uuid", uuid, "reason", result.Reason)
	}
	return err
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
		func(runner *run.Runner) error {
			return server.virtualMachines.Wake(ctx, runner, request.Uuid)
		})
	if failure != nil {
		return failure, nil
	}
	return wire.WakeVirtualMachine200JSONResponse{
		OperationAcceptedJSONResponse: wire.OperationAcceptedJSONResponse(operationToWire(operation)),
	}, nil
}

func (server *Server) TerminateVirtualMachine(ctx context.Context, request wire.TerminateVirtualMachineRequestObject) (wire.TerminateVirtualMachineResponseObject, error) {
	operation, failure := server.operation(ctx, request.Body, verbTerminateVirtualMachine, request.Uuid,
		func(runner *run.Runner) error {
			return server.virtualMachines.Terminate(ctx, runner, request.Uuid)
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
		func(runner *run.Runner) error {
			return server.virtualMachines.Resize(ctx, runner, request.Uuid, resize)
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
	build := func(runner *run.Runner) error {
		firecrackerUID, err := server.virtualMachines.FirecrackerUID(ctx, runner, request.Uuid)
		if err != nil {
			return err
		}
		rebuild.FirecrackerUID = firecrackerUID
		return server.virtualMachines.Rebuild(ctx, runner, request.Uuid, rebuild)
	}
	operation, failure := server.perform(ctx, request.Body.OperationId, verbRebuildVirtualMachine, request.Uuid, build)
	if failure != nil {
		return failure, nil
	}
	return wire.RebuildVirtualMachine200JSONResponse{
		OperationAcceptedJSONResponse: wire.OperationAcceptedJSONResponse(operationToWire(operation)),
	}, nil
}

// operation is the shared front of every verb whose whole request is the
// operation identifier: refuse what could not be replayed, then take it through
// perform like everything else.
func (server *Server) operation(
	ctx context.Context, body *wire.OperationRequest, verb string, uuid string, execute func(*run.Runner) error,
) (model.Operation, *errorResponse) {
	if body == nil || body.OperationId == "" {
		return model.Operation{}, missingOperationIdentifier()
	}
	return server.perform(ctx, body.OperationId, verb, uuid, execute)
}
