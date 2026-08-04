package snapshot

import (
	"context"
	"fmt"

	"github.com/frappe/boat/internal/thinpool"
)

// SnapshotVMParams identifies the source VM and where its snapshot LV(s) land.
// Atlas mints the snapshot names (atlas-snap-<id>, atlas-datasnap-<id>) and passes
// their device paths; the source disk LVs are derived from the UUID.
type SnapshotVMParams struct {
	UUID string
	// SnapshotRootfsPath is the /dev/atlas/<name> device path for the root
	// snapshot; its basename is the snapshot LV to create.
	SnapshotRootfsPath string
	// DataSnapshotRootfsPath is the data-disk snapshot's device path, empty when
	// the VM has no data disk — then only the root disk is snapshotted.
	DataSnapshotRootfsPath string
}

// SnapshotVMResult is the captured disk sizes the controller records.
type SnapshotVMResult struct {
	SizeBytes     int64
	DataSizeBytes int64 // 0 when the VM had no data disk
}

// SnapshotVM takes an LVM thin CoW snapshot of a Stopped VM's disk(s). Disk-only —
// no Firecracker memory state; the caller guarantees the VM is Stopped, so the
// disk is cleanly unmounted and the snapshot is consistent. Instant and O(1): the
// snapshot shares the VM disk's blocks until one side is written. A pure host op,
// no jail interaction. Idempotent: re-running reuses the existing snapshot LV.
//
// Ports scripts/snapshot-vm.py.
func SnapshotVM(ctx context.Context, cmd commands, params SnapshotVMParams) (SnapshotVMResult, error) {
	disk := "atlas-vm-" + params.UUID
	snapshotName := thinpool.NameFromDevice(params.SnapshotRootfsPath)

	// The origin must be on this host — a missing disk means the wrong UUID or the
	// wrong host, and is worth failing loudly for.
	if !thinpool.Exists(ctx, cmd, disk) {
		return SnapshotVMResult{}, fmt.Errorf(
			"disk LV not found for %s (%s); provision the VM first", params.UUID, disk,
		)
	}
	// A thin snapshot is free up front but every later CoW write allocates; do not
	// snapshot an almost-full pool.
	tooFull, err := thinpool.TooFull(ctx, cmd)
	if err != nil {
		return SnapshotVMResult{}, err
	}
	if tooFull {
		return SnapshotVMResult{}, fmt.Errorf("thin pool %s too full for a safe snapshot", thinpool.Pool)
	}

	if err := thinpool.SnapshotInto(ctx, cmd, disk, snapshotName); err != nil {
		return SnapshotVMResult{}, err
	}

	// The data disk (the root disk's peer) when the VM has one — same instant CoW
	// thin snapshot. A missing data disk is tolerated (the row claimed one but the
	// LV is gone): root is still captured. Its size is read here, and the root's
	// LAST, matching the Python's evaluation order at result construction.
	var dataSize int64
	if params.DataSnapshotRootfsPath != "" {
		dataDisk := "atlas-data-" + params.UUID
		if thinpool.Exists(ctx, cmd, dataDisk) {
			dataSnapshot := thinpool.NameFromDevice(params.DataSnapshotRootfsPath)
			if err := thinpool.SnapshotInto(ctx, cmd, dataDisk, dataSnapshot); err != nil {
				return SnapshotVMResult{}, err
			}
			dataSize, err = thinpool.SizeBytes(ctx, cmd, dataSnapshot)
			if err != nil {
				return SnapshotVMResult{}, err
			}
		}
	}
	size, err := thinpool.SizeBytes(ctx, cmd, snapshotName)
	if err != nil {
		return SnapshotVMResult{}, err
	}
	return SnapshotVMResult{SizeBytes: size, DataSizeBytes: dataSize}, nil
}
