package vm

import (
	"context"

	"github.com/frappe/boat/internal/run"
)

// Terminate deletes everything this host holds for a VM.
//
// Idempotent, and that is the requirement rather than a nicety: terminate is
// the verb most likely to be retried after a partial failure, so every step
// tolerates having already been done and the whole thing succeeds on a VM that
// is entirely gone. There is no separate cleanup path — a repair mode is the
// code that only runs after something went wrong, and it is never right when it
// finally does.
//
// The ORDER is the part that matters. The unit goes first: while a guest is
// running, its Firecracker holds the root volume open, and an lvremove against
// a volume with an open file descriptor fails. Then the VM directory, which
// takes the whole jail tree with it — kernel, config, API socket, and the block
// NODES that point at the volumes. The nodes are only pointers; the volumes
// themselves live in the pool, outside the tree, and are removed last.
//
// What is deliberately NOT removed: this VM's snapshots. Their names are not
// derivable from this UUID, they are separate records with their own delete
// path, and guessing at them here is how a terminate takes someone else's
// bytes.
func (manager *Manager) Terminate(ctx context.Context, runner *run.Runner, uuid string) error {
	commands := manager.commandsFor(runner)
	files := manager.filesFor(uuid)
	// Unchecked: the unit may already be gone, may never have started, or may
	// have been disabled by an earlier attempt. Its stop is what runs the
	// networking teardown in ExecStopPost, so this is the step that unwinds the
	// host's netns, veth and proxy-NDP for the VM.
	commands.RunUnchecked(ctx, "sudo systemctl disable --now {}", files.unit)
	if _, err := commands.Run(ctx, "sudo rm -rf {}", files.directory); err != nil {
		return err
	}
	// A migrated VM ran on a dm-clone that lingers after its collapse and holds
	// the plain volume BUSY, so the removes below would fail with "used by
	// another device". The guest is stopped by now and its descriptor released,
	// which is the only moment removing the clone is safe. A VM that never
	// migrated has none and this is a pair of no-ops.
	manager.convergeClone(ctx, commands, uuid)
	if err := rootDisk(uuid).remove(ctx, commands); err != nil {
		return err
	}
	// The data disk lives in the pool too, outside the directory just removed,
	// so it needs its own lvremove. A no-op for a VM that never had one.
	return dataDisk(uuid).remove(ctx, commands)
}
