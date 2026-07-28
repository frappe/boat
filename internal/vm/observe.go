package vm

import (
	"context"
	"strings"

	"github.com/frappe/boat/internal/fcattach"
	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/run"
)

const (
	unitActiveStateProperty = "ActiveState"
	unitSubStateProperty    = "SubState"

	unitActive   = "active"
	unitInactive = "inactive"
	unitFailed   = "failed"
)

// Observe reads this VM's state off the host: the unit's ActiveState and
// SubState, the on-disk markers, and — for a unit claiming to hold a live
// Firecracker — the Firecracker itself.
//
// This is the inversion the split is for. Atlas used to set a VM's status from
// whether its Task succeeded, which recorded what the controller had asked for
// rather than what the host did — a VM that died an hour after a successful
// start stayed Running forever, and a VM that came up after a failed start
// stayed Stopped. Nothing here infers status from a command having succeeded;
// the only claims made are the ones the host answered.
//
// Reading systemd alone was one layer short of that. `ActiveState=active` is
// systemd's claim about a process it supervises, and it is what the unit
// believed the last time it looked — so a Firecracker that died between two
// looks reads as Running, and a guest frozen through the Firecracker API reads
// as Running too, because pause never touches the unit. Both are answered by
// asking the process, which is what confirmRunning does.
//
// When the host cannot be read at all, the record comes back StatusUnknown
// together with the error. Unknown means "I could not see", never "it is dead":
// the record is still worth persisting, because it carries the timestamp of the
// last time we tried.
func (manager *Manager) Observe(
	ctx context.Context, runner *run.Runner, uuid string,
) (model.VirtualMachine, error) {
	observed, err := manager.readUnitAndMarkers(ctx, runner, uuid)
	if err != nil || observed.ObservedStatus != model.StatusRunning {
		return observed, err
	}
	return manager.confirmRunning(ctx, runner, observed)
}

// readUnitAndMarkers is the half of an observation systemd and the disk can
// answer between them. It is enough for every VM that is not claiming to be up.
func (manager *Manager) readUnitAndMarkers(
	ctx context.Context, runner *run.Runner, uuid string,
) (model.VirtualMachine, error) {
	commands := manager.commandsFor(runner)
	files := manager.filesFor(uuid)
	observed := model.VirtualMachine{UUID: uuid, ObservedAt: manager.clock.Now()}
	output, err := commands.Run(
		ctx, "systemctl show {} --property=ActiveState --property=SubState", files.unit,
	)
	if err != nil {
		observed.ObservedStatus = model.StatusUnknown
		return observed, err
	}
	properties := parseUnitProperties(output)
	observed.UnitActiveState = properties[unitActiveStateProperty]
	observed.UnitSubState = properties[unitSubStateProperty]
	observed.Sleeping = commands.OK(ctx, "sudo test -f {}", files.sleepingMarker)
	observed.HasMemorySnapshot = manager.memorySnapshotMarkerPresent(ctx, commands, files)
	observed.ObservedStatus = statusOf(observed)
	return observed, nil
}

// confirmRunning asks the Firecracker itself, because an active unit is a claim
// about a process and only that process can confirm it.
//
// Two facts systemd cannot carry come back from here. A paused guest keeps its
// unit active — pause goes through the Firecracker API and the VMM stays
// resident, which is why pause frees CPU and not memory — so Running and Paused
// are one state as far as the unit is concerned, and this call is the only thing
// that separates them. And a unit that says active over a Firecracker that is
// gone is a unit describing a process that is not there; taking its word is the
// same "status from what we asked for" the whole split exists to end.
//
// Nothing here concludes a VM is gone. A socket that does not answer makes the
// two host facts disagree, and disagreement is Unknown — "I could not see" —
// which is the one status the reconciler deliberately never acts on. Stopped
// would be a claim nothing observed, and Failed would be a claim about a unit
// that did not fail.
func (manager *Manager) confirmRunning(
	ctx context.Context, runner *run.Runner, observed model.VirtualMachine,
) (model.VirtualMachine, error) {
	process, live, err := manager.liveness(ctx, runner, observed.UUID)
	if err != nil {
		observed.ObservedStatus = model.StatusUnknown
		return observed, err
	}
	if !live {
		observed.ObservedStatus = model.StatusUnknown
		return observed, nil
	}
	observed.FirecrackerPID = process.Pid
	observed.ObservedStatus = guestStatusOf(process.State)
	return observed, nil
}

// guestStatusOf maps Firecracker's own word for the guest onto Boat's status.
//
// Everything it does not recognise is Unknown, and that includes Firecracker's
// third state: a VMM that is up with no guest in it yet is a VM mid-launch,
// which is not evidence for either answer — the same reading `activating` gets
// below. A state string a later Firecracker invents lands here too, and Unknown
// is the safe direction for it: Unknown is a status nothing acts on, while
// guessing Running would put a fact nobody observed into the export.
func guestStatusOf(state string) model.VirtualMachineStatus {
	switch state {
	case fcattach.StateRunning:
		return model.StatusRunning
	case fcattach.StatePaused:
		return model.StatusPaused
	default:
		return model.StatusUnknown
	}
}

// parseUnitProperties reads `systemctl show`'s KEY=value lines. Values may
// contain '=' themselves, so only the first one separates.
func parseUnitProperties(output string) map[string]string {
	properties := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			properties[key] = strings.TrimSpace(value)
		}
	}
	return properties
}

// statusOf maps what systemd and the markers said to Boat's status.
//
// The sleeping marker wins over the unit state, because a sleeping VM's unit is
// inactive by construction and the marker is what distinguishes "parked, an
// inbound SYN wakes it" from "stopped, and stays stopped". A unit state we do
// not recognise — activating, deactivating, a host mid-transition — is Unknown
// rather than a guess in either direction.
//
// Running is the one answer here that is provisional: it says the unit claims a
// live Firecracker, and confirmRunning is what turns that claim into Running,
// Paused or Unknown. It is spelled this way round so that the probe runs for
// exactly the VMs whose status depends on it — the marker still outranks
// everything, and a stopped VM costs no round trip to a socket that is not there.
func statusOf(observed model.VirtualMachine) model.VirtualMachineStatus {
	switch {
	case observed.Sleeping:
		return model.StatusSleeping
	case observed.UnitActiveState == unitActive:
		return model.StatusRunning
	case observed.UnitActiveState == unitFailed:
		// Distinct from Stopped, and the distinction is the point: a VM whose
		// unit failed is one whose guest died or whose start limit tripped, and
		// reading that as Stopped makes it indistinguishable from a VM an
		// operator asked to stop. Conflating the two is exactly what setting
		// status from a command's success used to do.
		return model.StatusFailed
	case observed.UnitActiveState == unitInactive:
		return model.StatusStopped
	default:
		return model.StatusUnknown
	}
}
