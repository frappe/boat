package vm

import (
	"context"

	"github.com/frappe/boat/internal/run"
)

// Start brings the VM's unit up. Idempotent: `systemctl start` on a unit that
// is already running is a no-op, so a retried start is not an error and the
// caller never has to ask whether the VM is up before asking for it to be up.
//
// When a previous stop left a complete memory snapshot behind (the READY marker
// next to the vmstate/mem pair), the unit resumes the guest from it —
// milliseconds instead of a cold boot. Nothing here decides that: the launcher
// sees the marker and starts Firecracker idle, and the unit's ExecStartPost
// loads the snapshot and resumes. The one wrinkle this function owns is the
// restore that fails; see startAgainAfterFailedRestore.
//
// Reports whether the guest came back from the memory snapshot or cold-booted.
// On any error the flag is meaningless and is returned false.
func (manager *Manager) Start(ctx context.Context, runner *run.Runner, uuid string) (bool, error) {
	commands := manager.commandsFor(runner)
	files := manager.filesFor(uuid)
	restoring := manager.memorySnapshotMarkerPresent(ctx, commands, files)
	if _, err := commands.Run(ctx, "sudo systemctl start {}", files.unit); err != nil {
		if !manager.restoreFailed(ctx, commands, files, restoring) {
			return false, err
		}
		if err := manager.startAgainAfterFailedRestore(ctx, commands, files); err != nil {
			return false, err
		}
		restoring = false
	}
	if err := manager.confirmActive(ctx, commands, files); err != nil {
		return false, err
	}
	return restoring, nil
}

// restoreFailed reports the one start failure that must not be handed back to
// the caller: the marker was present before the start and is gone after it.
// Only a restore attempt consumes the marker, so that pair is the signature of
// a restore that failed, as distinct from a boot that failed on its own merits.
// Any other failure is a real failure and is reported as one.
func (manager *Manager) restoreFailed(
	ctx context.Context, commands commands, files virtualMachineFiles, restoring bool,
) bool {
	return restoring && !manager.memorySnapshotMarkerPresent(ctx, commands, files)
}

// startAgainAfterFailedRestore cancels the relaunch that Restart=always has
// already scheduled, and starts the unit synchronously instead.
//
// Without this, a failed restore leaves the operation Failed while systemd
// brings the VM up five seconds later behind the controller's back — a Failed
// task sitting next to a running VM, which is the worst state to be in because
// nothing reconciles it. reset-failed drops the pending restart; the marker is
// already gone by now, so this start cold-boots. Exactly one retry: a second
// failure is a failure to boot, not a failure to restore.
func (manager *Manager) startAgainAfterFailedRestore(
	ctx context.Context, commands commands, files virtualMachineFiles,
) error {
	if _, err := commands.Run(ctx, "sudo systemctl reset-failed {}", files.unit); err != nil {
		return err
	}
	_, err := commands.Run(ctx, "sudo systemctl start {}", files.unit)
	return err
}

// confirmActive re-reads the unit after the start, because `systemctl start`
// returns when the start job is done and not when the service has settled. A
// guest that failed its own boot surfaces here as a failed start, instead of
// being reported as a success that the next Observe quietly contradicts.
func (manager *Manager) confirmActive(
	ctx context.Context, commands commands, files virtualMachineFiles,
) error {
	_, err := commands.Run(ctx, "sudo systemctl is-active {}", files.unit)
	return err
}
