package api

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/frappe/boat/internal/migration"
	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/netapply/vmnetwork"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/store"
	"github.com/frappe/boat/internal/wire"
)

// The cross-host migration saga's host surface (spec/33-boat.md §8, WO-4). Atlas
// is the sole orchestrator: it drives the phases one RPC at a time in a star and
// the two hosts never talk on the control plane, so each phase is a stateless,
// idempotent step this host runs when named and reports a typed result for. The
// phase logic lives in internal/migration; this file is only the wire↔phase glue
// — the dispatch, the two callbacks, and the result mapping.
//
// Every phase is keyed off HOST ARTIFACTS (dmsetup info, qemu-nbd pidfiles,
// /sys/block/nbdN/pid), never off stored state, so a lost-task re-entry
// re-derives the same devices from the UUID alone. Two things set these verbs
// apart from the lifecycle nine, and both are why they do NOT reuse `perform`:
// a target-side phase runs before the VM exists on this host, and a source-side
// phase runs as it is being retired — so the Exists gate and the post-verb
// observe that `perform` wraps every lifecycle verb in do not apply here.

// The mutating phases, in the spelling Atlas's boat_client and this host share.
// The poll-only Hydrating phase is GET .../migrate/hydration, not one of these.
const (
	phaseExportSource    = "export-source"
	phaseExportBase      = "export-base"
	phaseCloneTarget     = "clone-target"
	phaseReceiveBase     = "receive-base"
	phaseInjectIdentity  = "inject-identity"
	phaseCollapseClone   = "collapse-clone"
	phaseForwardUp       = "forward-up"
	phaseSourceForward   = "source-forward"
	phaseTargetReceive   = "target-receive"
	phaseForwardDown     = "forward-down"
	phaseWithdrawPrivate = "withdraw-private"
	phaseCleanupSource   = "cleanup-source"
)

// MigrateVirtualMachine runs one mutating phase of the migration saga.
//
// It claims on the operation identifier, takes the VM's turn and journals a
// terminal record exactly as the lifecycle verbs do — so a retried Atlas Task
// returns its first result and two phases for one UUID never overlap — but
// through performMigrationPhase rather than perform, because a migration phase
// acts on a VM that this host may not hold yet (a target clone) or is retiring (a
// source cleanup). An unknown phase is refused at the boundary, before an
// operation is claimed, so a typo does not burn an identifier.
func (server *Server) MigrateVirtualMachine(ctx context.Context, request wire.MigrateVirtualMachineRequestObject) (wire.MigrateVirtualMachineResponseObject, error) {
	if request.Body == nil || request.Body.OperationId == "" {
		return missingOperationIdentifier(), nil
	}
	verb, known := migrationVerb(request.Phase)
	if !known {
		return badRequest("Unknown migration phase " + request.Phase + "; this host runs no such phase."), nil
	}
	body := *request.Body
	work := func(ctx context.Context, runner *run.Runner) (model.OperationResult, error) {
		return server.runMigrationPhase(ctx, runner, request.Uuid, request.Phase, body)
	}
	operation, failure := server.performMigrationPhase(ctx, request.Body.OperationId, verb, request.Uuid, work)
	if failure != nil {
		return failure, nil
	}
	return wire.MigrateVirtualMachine200JSONResponse{
		OperationAcceptedJSONResponse: wire.OperationAcceptedJSONResponse(operationToWire(operation)),
	}, nil
}

// GetMigrationHydration is the Hydrating phase — a poll the controller drives
// once per tick. It is a plain read path, NOT a journaled operation: `perform`
// writes one terminal record per operation_id, and a per-tick poll would either
// bury the journal or need a fresh id every tick, so this carries no identifier,
// claims nothing and takes no turn — the same shape as GET /export. It still
// enables hydration on first touch (an idempotent dmsetup message inside the
// phase) and reads the current percent, but writes nothing down.
func (server *Server) GetMigrationHydration(ctx context.Context, request wire.GetMigrationHydrationRequestObject) (wire.GetMigrationHydrationResponseObject, error) {
	if failure := server.refuseMalformedUUID(request.Uuid); failure != nil {
		return failure, nil
	}
	// The trace of a read is discarded: it belongs to no operation record, and this
	// runs every controller tick for the life of a migration.
	runner := server.newRunner(io.Discard)
	result, err := server.pollHydration(ctx, runner, request.Uuid, optional(request.Params.CloneDevice))
	if err != nil {
		return internalFault("This VM's hydration state could not be read.", err), nil
	}
	return wire.GetMigrationHydration200JSONResponse{
		HydrationPercent: result.HydrationPercent,
		SourceHealthy:    result.SourceHealthy,
	}, nil
}

// migrationVerb maps a phase to its operation-record verb and reports whether the
// phase is one this host runs. The verb reads the same string in the journal and
// in Atlas's boat_client table — migrate-<phase> — and this switch is the single
// list of the phases the POST endpoint accepts.
func migrationVerb(phase string) (string, bool) {
	switch phase {
	case phaseExportSource, phaseExportBase, phaseCloneTarget, phaseReceiveBase,
		phaseInjectIdentity, phaseCollapseClone, phaseForwardUp, phaseSourceForward,
		phaseTargetReceive, phaseForwardDown, phaseWithdrawPrivate, phaseCleanupSource:
		return "migrate-" + phase, true
	default:
		return "", false
	}
}

// performMigrationPhase is perform without the two things a migration phase must
// not have: the Exists gate (a target phase runs before the VM is here) and the
// post-verb observe (there is no coherent VM to observe mid-migration, and a
// source cleanup just destroyed the one there was). It keeps everything else —
// the idempotent claim, the per-UUID turn, the terminal record — so a phase is
// replay-safe, serialized against every other verb for the VM, and journalled.
func (server *Server) performMigrationPhase(
	ctx context.Context, identifier, verb, uuid string, execute hostWork,
) (model.Operation, *errorResponse) {
	if failure := server.refuseMalformedUUID(uuid); failure != nil {
		return model.Operation{}, failure
	}
	operation, claimed, err := server.operations.ClaimOperation(identifier, verb, uuid)
	switch {
	case errors.Is(err, store.ErrOperationConflict):
		return operation, conflict("Operation " + identifier + " is already recorded against different work.")
	case err != nil:
		return operation, internalFault("The operation could not be claimed.", err)
	case !claimed:
		return operation, nil
	}
	recorded, failure := operation, (*errorResponse)(nil)
	turnError := server.reconciler.Do(ctx, uuid, func(ctx context.Context) error {
		var trace bytes.Buffer
		runner := server.newRunner(&trace)
		result, verbError := execute(ctx, runner)
		recorded, failure = server.record(operation, &trace, result, verbError)
		return nil
	})
	if turnError != nil {
		return server.abandoned(operation, uuid, turnError)
	}
	return recorded, failure
}

// executeMigrationPhase is the default runMigrationPhase: it dispatches the named
// phase to internal/migration with a real runner, wires the two callbacks, and
// maps the phase's typed result onto the operation record. It is a Server field
// (like newRunner and hostFacts) so a handler test — which has no qemu-nbd, no
// dmsetup and no root — substitutes it; the result mappers below stay pure and
// are unit-tested on their own.
func (server *Server) executeMigrationPhase(
	ctx context.Context, runner *run.Runner, uuid, phase string, body wire.MigrateRequest,
) (model.OperationResult, error) {
	switch phase {
	case phaseExportSource:
		result, err := migration.ExportSource(ctx, runner, uuid, migration.ExportSourceParams{
			BindAddress: optional(body.BindAddress),
		})
		if err != nil {
			return nil, err
		}
		return exportSourceResult(result), nil

	case phaseExportBase:
		result, err := migration.ExportBase(ctx, runner, uuid, migration.ExportBaseParams{
			ImageName:   optional(body.ImageName),
			BindAddress: optional(body.BindAddress),
		})
		if err != nil {
			return nil, err
		}
		return exportBaseResult(result), nil

	case phaseCloneTarget:
		result, err := migration.CloneTarget(ctx, runner, uuid, migration.CloneTargetParams{
			ImageName:  optional(body.ImageName),
			DiskGB:     optional(body.DiskGb),
			DataDiskGB: optional(body.DataDiskGb),
			SourceHost: optional(body.SourceHost),
		})
		if err != nil {
			return nil, err
		}
		return cloneTargetResult(result), nil

	case phaseReceiveBase:
		return nil, migration.ReceiveBase(ctx, runner, uuid, migration.ReceiveBaseParams{
			ImageName:  optional(body.ImageName),
			DiskGB:     optional(body.DiskGb),
			SourceHost: optional(body.SourceHost),
			Phase:      string(optional(body.BasePhase)),
		})

	case phaseInjectIdentity:
		identity := identityFrom(body.Identity)
		inject := func(ctx context.Context, device string) error {
			return server.virtualMachines.InjectIdentity(ctx, runner, device, uuid, identity)
		}
		return nil, migration.InjectIdentity(ctx, runner, uuid, inject)

	case phaseCollapseClone:
		return nil, migration.CollapseClone(ctx, runner, uuid, migration.CollapseCloneParams{
			DataDiskGB: optional(body.DataDiskGb),
		})

	case phaseForwardUp:
		result, err := migration.ForwardUp(ctx, runner, uuid, migration.ForwardUpParams{
			Role:               string(optional(body.Role)),
			SourceHost:         optional(body.SourceHost),
			VirtualMachineIPv6: optional(body.VirtualMachineIpv6),
		})
		if err != nil {
			return nil, err
		}
		return forwardUpResult(result), nil

	case phaseSourceForward:
		result, err := migration.SourceForward(ctx, runner, uuid, migration.SourceForwardParams{
			VirtualMachineIPv6: optional(body.VirtualMachineIpv6),
		})
		if err != nil {
			return nil, err
		}
		return model.OperationResult{"forwarding": result.Forwarding}, nil

	case phaseTargetReceive:
		result, err := migration.TargetReceive(ctx, runner, uuid, migration.TargetReceiveParams{
			VirtualMachineIPv6: optional(body.VirtualMachineIpv6),
		})
		if err != nil {
			return nil, err
		}
		return model.OperationResult{"receiving": result.Receiving}, nil

	case phaseForwardDown:
		result, err := migration.ForwardDown(ctx, runner, uuid, migration.ForwardDownParams{
			Role:               string(optional(body.Role)),
			VirtualMachineIPv6: optional(body.VirtualMachineIpv6),
		})
		if err != nil {
			return nil, err
		}
		return model.OperationResult{"down": result.Down}, nil

	case phaseWithdrawPrivate:
		// WithdrawPrivate touches only the local-ownership cache — no runner, no
		// UUID — but it still rides the claim/turn/record machinery for idempotent
		// replay and per-UUID serialization against the cutover it sequences before.
		return nil, migration.WithdrawPrivate(optional(body.PrivateAddress))

	case phaseCleanupSource:
		networkDown := func(ctx context.Context) error { return vmnetwork.Down(ctx, runner, uuid) }
		return nil, migration.CleanupSource(ctx, runner, uuid, migration.CleanupSourceParams{
			NBDPID:      optional(body.NbdPid),
			KeepAddress: optional(body.KeepAddress),
		}, networkDown)

	default:
		// Unreachable: MigrateVirtualMachine refuses an unknown phase before it ever
		// claims an operation, so this switch is only reached for a phase migrationVerb
		// already vouched for. Kept as a loud failure rather than a silent success.
		return nil, errors.New("migration: no host work for phase " + phase)
	}
}

// The result mappers turn a phase's typed Go result into the operation record's
// `result` map — the same payload the Python script's one ATLAS_RESULT= line
// carried, so Atlas parses a Task the same way whichever transport filled it. A
// field whose zero value means "not present" (a data disk that does not exist) is
// omitted rather than sent as 0, the same discipline sleepResult keeps: an absent
// key is not a false one.

func exportSourceResult(result migration.ExportSourceResult) model.OperationResult {
	values := model.OperationResult{
		"nbd_port":        result.NBDPort,
		"nbd_pid":         result.NBDPID,
		"root_size_bytes": result.RootSizeBytes,
	}
	if result.DataSizeBytes != 0 {
		values["data_size_bytes"] = result.DataSizeBytes
	}
	return values
}

func exportBaseResult(result migration.ExportBaseResult) model.OperationResult {
	return model.OperationResult{
		"nbd_port":        result.NBDPort,
		"nbd_pid":         result.NBDPID,
		"base_size_bytes": result.BaseSizeBytes,
		"meta_port":       result.MetaPort,
		"meta_pid":        result.MetaPID,
		"meta_size_bytes": result.MetaSizeBytes,
	}
}

func cloneTargetResult(result migration.CloneTargetResult) model.OperationResult {
	values := model.OperationResult{"root_clone_device": result.RootCloneDevice}
	if result.DataCloneDevice != "" {
		values["data_clone_device"] = result.DataCloneDevice
	}
	return values
}

func forwardUpResult(result migration.ForwardUpResult) model.OperationResult {
	return model.OperationResult{
		"tunnel_device": result.TunnelDevice,
		"up":            result.Up,
	}
}
