package snapshot

import (
	"context"
	"fmt"

	"github.com/frappe/boat/internal/paths"
)

// SnapshotStopParams selects the VM to stop and the uid its jailed Firecracker
// writes the snapshot under.
type SnapshotStopParams struct {
	UUID string
	// FirecrackerUID is the per-VM uid; the JAILED Firecracker (running as it) is
	// what writes the vmstate and memory files, so the snapshot directory is
	// chowned to it.
	FirecrackerUID int
}

// SnapshotStopResult says whether the memory snapshot was captured, and if not,
// why — the only record of WHY the next start will cold-boot, which is a host
// fault an operator can fix.
type SnapshotStopResult struct {
	MemorySnapshot      bool
	Reason              string
	MemorySnapshotBytes int64
}

// SnapshotStopVM stops a VM, capturing its full memory state first so the next
// start resumes it in milliseconds instead of cold-booting (60-120s to SSH):
// pause the vCPUs, PUT /snapshot/create, write the READY marker only once the
// pair is complete on disk, then systemctl stop.
//
// Any pre-flight or snapshot failure falls back to the plain stop and reports
// memory_snapshot=false with the reason — the VM always ends up Stopped; only the
// next start's speed differs. The fallback also clears any stale marker so a
// half-written snapshot can never be restored. A restored guest continues exactly
// where it paused (its clock is stale until NTP corrects it; it never observes a
// reboot). Ports scripts/snapshot-stop-vm.py.
func SnapshotStopVM(ctx context.Context, cmd commands, params SnapshotStopParams) (SnapshotStopResult, error) {
	vm := paths.ForVirtualMachine(params.UUID)

	// Pre-flights. Each miss takes the default path — the operator asked for a
	// stop, and a stop is what they get either way.
	reason, err := snapshotStopPreflight(ctx, cmd, vm)
	if err != nil {
		return SnapshotStopResult{}, err
	}
	if reason == "" {
		if err := createStopSnapshot(ctx, cmd, vm, params.FirecrackerUID); err != nil {
			reason = fmt.Sprintf("snapshot failed: %v", err)
		}
	}
	if reason != "" {
		return plainStop(ctx, cmd, vm, reason)
	}

	if _, err := cmd.Run(ctx, "sudo systemctl stop {}", vm.SystemdUnit()); err != nil {
		return SnapshotStopResult{}, err
	}
	memoryBytes, err := memoryFileSize(ctx, cmd, vm.MemorySnapshotMemory())
	if err != nil {
		return SnapshotStopResult{}, err
	}
	return SnapshotStopResult{MemorySnapshot: true, MemorySnapshotBytes: memoryBytes}, nil
}

// snapshotStopPreflight returns the reason a memory snapshot cannot be taken, or
// "" to proceed. A returned error is the host being unreadable at all (jq or df
// failing), which aborts hard rather than falling back — matching the Python,
// where those reads are not inside main's try.
func snapshotStopPreflight(ctx context.Context, cmd commands, vm paths.VirtualMachine) (string, error) {
	// A launcher generated before this feature always passes --config-file, so a
	// marker written now would strand the next start (a snapshot loaded over a
	// booted guest is refused). Re-provisioning regenerates the launcher.
	if !cmd.OK(ctx, "sudo grep -q snapshot/READY {}", vm.JailerLaunch()) {
		return "launcher predates memory snapshots; re-provision the VM to enable fast start", nil
	}
	// test -S, not the Python's plain existence check: the API endpoint is a unix
	// socket, and this matches vm/sleep.go's memory-capture preflight.
	if !cmd.OK(ctx, "sudo test -S {}", vm.APISocket()) {
		return "API socket missing; is the VM running?", nil
	}
	return checkMemoryFileSpace(ctx, cmd, vm)
}

// createStopSnapshot freezes the guest, writes the vmstate/memory pair, and then
// writes the marker LAST — the marker asserts a COMPLETE pair, and everything a
// later start does with the snapshot keys off it.
func createStopSnapshot(ctx context.Context, cmd commands, vm paths.VirtualMachine, firecrackerUID int) error {
	if err := installAndOwnMemoryDirectory(ctx, cmd, vm, firecrackerUID); err != nil {
		return err
	}
	if err := cmd.FirecrackerAPI(
		ctx, vm.APISocketDirectory(), vm.APISocketName(), "PATCH", "/vm", pausedStateBody,
	); err != nil {
		return err
	}
	if err := writeAndVerifyMemoryPair(ctx, cmd, vm); err != nil {
		return err
	}
	_, err := cmd.Run(ctx, "sudo touch {}", vm.MemorySnapshotMarker())
	return err
}

// plainStop is the default path: no marker may survive (a partial snapshot must
// never be restored), then the ordinary unit stop.
func plainStop(ctx context.Context, cmd commands, vm paths.VirtualMachine, reason string) (SnapshotStopResult, error) {
	if _, err := cmd.Run(ctx, "sudo rm -f {}", vm.MemorySnapshotMarker()); err != nil {
		return SnapshotStopResult{}, err
	}
	if _, err := cmd.Run(ctx, "sudo systemctl stop {}", vm.SystemdUnit()); err != nil {
		return SnapshotStopResult{}, err
	}
	return SnapshotStopResult{MemorySnapshot: false, Reason: reason}, nil
}
