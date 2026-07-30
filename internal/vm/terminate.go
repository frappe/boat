package vm

import (
	"context"
	"fmt"

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
// a volume with an open file descriptor fails. Then the parked networking, while
// the sidecar naming this VM's address is still on disk. Then the VM directory,
// which takes the whole jail tree with it — kernel, config, API socket, and the
// block NODES that point at the volumes. The nodes are only pointers; the
// volumes themselves live in the pool, outside the tree, and are removed last.
//
// What is deliberately NOT removed: this VM's snapshots. Their names are not
// derivable from this UUID, they are separate records with their own delete
// path, and guessing at them here is how a terminate takes someone else's
// bytes.
func (manager *Manager) Terminate(ctx context.Context, runner *run.Runner, uuid string) error {
	commands := manager.commandsFor(runner)
	files := manager.filesFor(uuid)
	// Unchecked: the unit may already be gone, may never have started, or may
	// have been disabled by an earlier attempt. For a RUNNING VM its stop is what
	// runs the networking teardown in ExecStopPost, which is what unwinds the
	// host's netns, veth and proxy-NDP — and for a stopped or sleeping one it
	// runs nothing at all, which is what the step below exists for.
	commands.RunUnchecked(ctx, "sudo systemctl disable --now {}", files.unit)
	if err := manager.retirePark(ctx, runner, uuid); err != nil {
		return err
	}
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

// retirePark unwinds the parked networking BEFORE the tree that names it is
// removed, which is the whole of why it is a step of its own.
//
// `systemctl disable --now` on an already-inactive unit does not re-run
// ExecStopPost, so a stopped or sleeping VM's networking is never torn down by
// the line above — and `rm -rf` takes network.env first, after which this host
// cannot name the address to withdraw at all. A terminate in the other order
// therefore leaves a permanent forward-chain DROP on every inbound SYN to a
// /128 Atlas is free to hand to the next VM. This is the ordering
// scripts/terminate-vm.py already had, ported rather than invented.
//
// A failure here fails the terminate, and does so while the tree is still on
// disk: the retry re-reads the sidecar and finishes the job, where carrying on
// would destroy the only record of what was left behind. The Python's guard on
// network.env still existing is not needed — the counter and the rule are named
// after the UUID rather than the address, so a VM whose sidecar is already gone
// still has its trap removed, and only the route and the neighbour entry go
// unnamed.
//
// What this cannot do is the rest of a teardown: the namespace, the veth, the
// tap and the per-VM forward rules belong to the Python vm-network-down hook the
// unit fires, and a VM whose unit failed its stop still carries them until WO-3
// gives Boat that hook in Go.
func (manager *Manager) retirePark(ctx context.Context, runner *run.Runner, uuid string) error {
	if err := manager.retire(ctx, runner, uuid); err != nil {
		return fmt.Errorf("the parked networking of %s could not be withdrawn: %w", uuid, err)
	}
	return nil
}
