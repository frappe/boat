package backup

import (
	"context"
	"fmt"
	"path"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/thinpool"
)

// RestoreSnapshotParams is the snapshot's name and the object plan, each item
// carrying its presigned GET url and the expected sha256 of the compressed bytes.
type RestoreSnapshotParams struct {
	SnapshotName string
	Objects      []BackupObject
}

// RestoreSnapshotResult names the artifacts rehydrated, e.g. ["rootfs", "data"].
type RestoreSnapshotResult struct {
	Objects []string
}

// RestoreSnapshotS3 rehydrates a snapshot's on-host artifacts from S3 via
// controller-presigned GET URLs. The inverse of UploadSnapshotS3: recreate the
// disk LV(s) the snapshot row already names and, for a warm snapshot, the memory
// pair + host signature. After this the snapshot is fully local again, so the
// ordinary restore/clone paths work unchanged.
//
// Each object: curl the compressed bytes to a temp file, VERIFY its sha256 BEFORE
// decompressing (the integrity gate — corrupt bytes never reach a decompressor
// pointed at a live LV), then decompress straight onto the recreated LV (--sparse,
// so a fresh thin LV stays thin) or into the memory file. Idempotent: a block LV
// is removed and recreated clean each time. Ports scripts/restore-snapshot-s3.py.
func RestoreSnapshotS3(ctx context.Context, cmd commands, params RestoreSnapshotParams) (RestoreSnapshotResult, error) {
	if len(params.Objects) == 0 {
		return RestoreSnapshotResult{}, fmt.Errorf("no objects to restore")
	}
	// A block object recreates a thin LV, which allocates from the pool; refuse a
	// pool already too full to take it. Only read the pool when there is a block
	// object — a warm memory-only rehydrate needs no pool room.
	if hasBlockObject(params.Objects) {
		tooFull, err := thinpool.TooFull(ctx, cmd)
		if err != nil {
			return RestoreSnapshotResult{}, err
		}
		if tooFull {
			return RestoreSnapshotResult{}, fmt.Errorf("thin pool %s too full to restore into", thinpool.Pool)
		}
	}

	work := paths.AtlasRoot + "/tmp/s3-restore-" + params.SnapshotName
	if _, err := cmd.Run(ctx, "sudo rm -rf {}", work); err != nil {
		return RestoreSnapshotResult{}, err
	}
	if err := cmd.InstallDirectory(ctx, work, "0700"); err != nil {
		return RestoreSnapshotResult{}, err
	}

	restoreErr := restoreAll(ctx, cmd, params.Objects, work)
	// Always sweep the working directory (the finally); a cleanup error is only
	// reported when the restore itself succeeded.
	if _, err := cmd.Run(ctx, "sudo rm -rf {}", work); err != nil && restoreErr == nil {
		return RestoreSnapshotResult{}, err
	}
	if restoreErr != nil {
		return RestoreSnapshotResult{}, restoreErr
	}
	// One sync at the end, after every LV has been written, so a crash right after
	// this verb reports success cannot leave the recreated disks half on disk.
	if _, err := cmd.Run(ctx, "sudo sync"); err != nil {
		return RestoreSnapshotResult{}, err
	}

	names := make([]string, len(params.Objects))
	for index, object := range params.Objects {
		names[index] = object.Name
	}
	return RestoreSnapshotResult{Objects: names}, nil
}

func restoreAll(ctx context.Context, cmd commands, objects []BackupObject, work string) error {
	for _, object := range objects {
		if err := restoreOne(ctx, cmd, object, work); err != nil {
			return err
		}
	}
	return nil
}

// restoreOne downloads one object, verifies its sha256, and writes it to its
// destination. The verify is BEFORE any decompress — the whole point of the gate.
func restoreOne(ctx context.Context, cmd commands, object BackupObject, work string) error {
	temp := work + "/" + object.ObjectName
	if _, err := cmd.Run(ctx, "sudo curl --fail --silent --show-error --output {} {}", temp, object.URL); err != nil {
		return err
	}
	// sha256sum -c - reads "<digest>  <path>" on stdin; two spaces, its own format.
	if _, err := cmd.Input(ctx, object.SHA256+"  "+temp, "sudo sha256sum -c -"); err != nil {
		return err
	}
	if object.Block {
		if err := restoreBlock(ctx, cmd, object, temp); err != nil {
			return err
		}
	} else if err := restoreFile(ctx, cmd, object, temp); err != nil {
		return err
	}
	_, err := cmd.Run(ctx, "sudo rm -f {}", temp)
	return err
}

// restoreBlock recreates the LV clean (so a re-restore has no stale blocks), then
// decompresses the image straight onto it. --sparse skips zero runs, keeping the
// thin LV thin.
func restoreBlock(ctx context.Context, cmd commands, object BackupObject, temp string) error {
	if object.DiskGigabytes == 0 {
		return fmt.Errorf("block object %s has no disk size to recreate %s", object.Name, object.Source)
	}
	name := thinpool.NameFromDevice(object.Source)
	if err := thinpool.Remove(ctx, cmd, name); err != nil { // no-op if absent; refuses protected LVs
		return err
	}
	if err := thinpool.CreateThin(ctx, cmd, name, object.DiskGigabytes); err != nil {
		return err
	}
	_, err := cmd.Run(ctx, "sudo zstd -d -q -f --sparse -o {} {}", thinpool.DevicePath(name), temp)
	return err
}

// restoreFile writes a warm memory-pair file (root:root 0644, matching the warm
// capture's durable staging) into the recreated memory directory.
func restoreFile(ctx context.Context, cmd commands, object BackupObject, temp string) error {
	if err := cmd.InstallDirectory(ctx, path.Dir(object.Source), "0755"); err != nil {
		return err
	}
	if object.Compress {
		if _, err := cmd.Run(ctx, "sudo zstd -d -q -f -o {} {}", object.Source, temp); err != nil {
			return err
		}
	} else if _, err := cmd.Run(ctx, "sudo cp {} {}", temp, object.Source); err != nil {
		return err
	}
	if _, err := cmd.Run(ctx, "sudo chown root:root {}", object.Source); err != nil {
		return err
	}
	_, err := cmd.Run(ctx, "sudo chmod 0644 {}", object.Source)
	return err
}

func hasBlockObject(objects []BackupObject) bool {
	for _, object := range objects {
		if object.Block {
			return true
		}
	}
	return false
}
