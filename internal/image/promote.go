package image

import (
	"context"
	"fmt"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/thinpool"
)

// PromoteSnapshotParams mirrors promote-snapshot-image.py's PromoteSnapshotInputs
// field for field. The snapshot device path is the dd source; the base image name
// becomes the atlas-image-<name> LV; the source image is where the kernel already
// lives (it is hard-linked, not re-exported).
type PromoteSnapshotParams struct {
	SnapshotRootfsPath string // the snapshot's /dev/atlas/<name> device path (the dd source)
	ImageName          string // the base image name; the LV becomes atlas-image-<image_name>
	DiskGigabytes      int    // size of the promoted base image LV (the snapshot's disk size)
	RootfsFilename     string // the new image's rootfs filename (presence sentinel in the image dir)
	SourceImage        string // the snapshot's source image name (where its kernel already lives)
	KernelFilename     string // the kernel filename to reuse (present in the source image's dir)
}

// PromoteSnapshotResult is what the controller records back: the created (or
// existing) atlas-image-<name> LV and its byte size.
type PromoteSnapshotResult struct {
	ImageLV   string
	SizeBytes int64
}

// PromoteSnapshotImage promotes a baked snapshot LV into a same-server base image,
// so new VMs provision from it via the ordinary `image` field instead of cloning a
// one-off snapshot. Same-server scope: the bytes never leave the host — snapshot LV
// to base-image LV is a local dd. Ports promote-snapshot-image.py's main().
// Idempotent: re-running with an existing target is a no-op.
//
// Two things make an image provisionable, and both are materialized so a promoted
// image looks exactly like a from-URL image to provision-vm.py:
//  1. the rootfs base LV atlas-image-<name>, dd'd from the snapshot LV;
//  2. the image directory holding the kernel (hard-linked from the SOURCE image)
//     and a rootfs presence sentinel (provision only stat-probes it; the real
//     bytes are the base LV).
func PromoteSnapshotImage(ctx context.Context, cmd commands, params PromoteSnapshotParams) (PromoteSnapshotResult, error) {
	// 1. The rootfs base LV: dd the snapshot LV into a read-only atlas-image-<name>
	//    LV — a standalone thin volume (own bytes, no origin) so it outlives the
	//    snapshot it was dd'd from. Idempotent — a no-op if the target LV exists.
	source := thinpool.NameFromDevice(params.SnapshotRootfsPath)
	imageLV, err := thinpool.ImportBaseImageFromLV(ctx, cmd, source, params.ImageName, params.DiskGigabytes)
	if err != nil {
		return PromoteSnapshotResult{}, err
	}

	// 2. The on-disk image directory, so provision-vm.py finds a kernel + a rootfs
	//    presence file exactly as it would for a synced image.
	imageDirectory := paths.ImageDirectory(params.ImageName)
	if err := cmd.InstallDirectory(ctx, imageDirectory, "0700"); err != nil {
		return PromoteSnapshotResult{}, err
	}

	// 2a. Hard-link the SOURCE image's kernel into this image's directory. Same
	//     filesystem (/var/lib/atlas), so `ln` always works; the byte-identical
	//     vmlinux is shared, not copied. `ln -f` is idempotent on a re-run.
	sourceKernel := paths.ImageDirectory(params.SourceImage) + "/" + params.KernelFilename
	destKernel := imageDirectory + "/" + params.KernelFilename
	if !cmd.OK(ctx, "sudo test -f {}", sourceKernel) {
		return PromoteSnapshotResult{}, fmt.Errorf(
			"source image kernel not found: %s; the snapshot's source image '%s' must be synced to this server first",
			sourceKernel, params.SourceImage,
		)
	}
	if _, err := cmd.Run(ctx, "sudo ln -f {} {}", sourceKernel, destKernel); err != nil {
		return PromoteSnapshotResult{}, err
	}

	// 2b. The rootfs presence sentinel. provision-vm.py only stat-probes this file;
	//     the disk bytes are the base LV (1). An empty-of-data file documents "the
	//     rootfs for this image is the LV of the same name", and satisfies the probe.
	sentinel := fmt.Sprintf(
		"# Local image promoted from snapshot %s. Rootfs is the LVM thin "+
			"volume %s, not this file (provision reads the LV).\n",
		source, imageLV,
	)
	if err := cmd.InstallFile(ctx, sentinel, imageDirectory+"/"+params.RootfsFilename, "0644"); err != nil {
		return PromoteSnapshotResult{}, err
	}

	sizeBytes, err := thinpool.SizeBytes(ctx, cmd, imageLV)
	if err != nil {
		return PromoteSnapshotResult{}, err
	}
	return PromoteSnapshotResult{ImageLV: imageLV, SizeBytes: sizeBytes}, nil
}
