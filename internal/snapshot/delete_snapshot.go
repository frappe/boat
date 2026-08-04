package snapshot

import (
	"context"
	"fmt"
	"strings"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/thinpool"
)

// DeleteSnapshotVMParams names the artifacts to remove: the snapshot LV device
// path(s), and — for a warm snapshot — its durable memory directory.
type DeleteSnapshotVMParams struct {
	// SnapshotRootfsPath is the root snapshot's /dev/atlas/<name> device path; its
	// basename is the snapshot LV to remove.
	SnapshotRootfsPath string
	// DataSnapshotRootfsPath is the data-disk snapshot's device path, empty when
	// the snapshot captured no data disk.
	DataSnapshotRootfsPath string
	// MemoryDirectory is a warm snapshot's durable vmstate/mem/host-signature
	// directory, empty for a cold snapshot. It MUST live under
	// /var/lib/atlas/snapshots — the guard keeps a malformed row from rm -rf'ing
	// anything else.
	MemoryDirectory string
}

// DeleteSnapshotVM removes a VM disk snapshot (and, for a warm snapshot, its
// durable memory artifacts). Idempotent: a missing LV or directory is a no-op. A
// pure host op — no jail interaction.
//
// A snapshot LV is an independent thin volume — removing it never affects the VM
// disk it was taken from, nor any clone made from it (clones are independent thin
// LVs once created). A clone jail holds hard LINKS to the memory pair, so removing
// the directory never breaks an already-provisioned clone — only future warm
// staging. Ports scripts/delete-snapshot-vm.py.
func DeleteSnapshotVM(ctx context.Context, cmd commands, params DeleteSnapshotVMParams) error {
	// thinpool.Remove is guarded (refuses pool/base-image LVs) and a no-op if
	// absent.
	if err := thinpool.Remove(ctx, cmd, thinpool.NameFromDevice(params.SnapshotRootfsPath)); err != nil {
		return err
	}
	if params.DataSnapshotRootfsPath != "" {
		if err := thinpool.Remove(ctx, cmd, thinpool.NameFromDevice(params.DataSnapshotRootfsPath)); err != nil {
			return err
		}
	}
	if params.MemoryDirectory != "" {
		// rm -rf is idempotent; the path guard keeps this from ever sweeping outside
		// the snapshots tree even if a row is malformed.
		if !strings.HasPrefix(params.MemoryDirectory, paths.SnapshotsDirectory+"/") {
			return fmt.Errorf(
				"memory directory must live under %s: %s", paths.SnapshotsDirectory, params.MemoryDirectory,
			)
		}
		if _, err := cmd.Run(ctx, "sudo rm -rf {}", params.MemoryDirectory); err != nil {
			return err
		}
	}
	return nil
}
