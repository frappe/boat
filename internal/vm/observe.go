package vm

import (
	"context"
	"strings"

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
// SubState, plus the on-disk markers.
//
// This is the inversion the split is for. Atlas used to set a VM's status from
// whether its Task succeeded, which recorded what the controller had asked for
// rather than what the host did — a VM that died an hour after a successful
// start stayed Running forever, and a VM that came up after a failed start
// stayed Stopped. Nothing here infers status from a command having succeeded;
// the only claims made are the ones the host answered.
//
// When the host cannot be read at all, the record comes back StatusUnknown
// together with the error. Unknown means "I could not see", never "it is dead":
// the record is still worth persisting, because it carries the timestamp of the
// last time we tried.
func (manager *Manager) Observe(
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

// statusOf maps what the host said to Boat's status.
//
// The sleeping marker wins over the unit state, because a sleeping VM's unit is
// inactive by construction and the marker is what distinguishes "parked, an
// inbound SYN wakes it" from "stopped, and stays stopped". A unit state we do
// not recognise — activating, deactivating, a host mid-transition — is Unknown
// rather than a guess in either direction.
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
