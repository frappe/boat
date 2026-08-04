package image

import (
	"context"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/thinpool"
)

// SyncImageParams mirrors sync-image.py's SyncImageInputs field for field. Every
// value is flat image data an operator reads off the image's spec (urls,
// filenames, sha256, disk-gb) — there are no controller-only required fields, so
// the verb stays operator-typable for break-glass. The two SHA256 digests are of
// the *packed* kernel and the *source squashfs*, not of the derived artifacts.
type SyncImageParams struct {
	ImageName      string // directory name under /var/lib/atlas/images
	KernelURL      string // HTTPS URL of the packed, zstd-compressed vmlinuz
	KernelFilename string // destination filename, e.g. vmlinux-6.1.141
	KernelSHA256   string // hex digest of the *packed* kernel artifact
	RootfsURL      string // HTTPS URL of the source squashfs
	RootfsFilename string // destination ext4 filename, e.g. ubuntu-24.04.ext4
	RootfsSHA256   string // hex digest of the *source squashfs*, not the ext4
	DefaultDiskGB  int    // size of the pristine ext4
	// GuestNetworkUnit is the server path to the guest atlas-network.service to
	// bake in. Empty means the controller's staged sidecar (StagedGuestNetworkUnit)
	// — the always-correct path for a controller-driven sync; a hand run may point
	// it at a real file.
	GuestNetworkUnit string
}

// SyncImageResult is small on purpose: sync-image.py prints only a human
// "Image <name> ready." line and the controller parses nothing back. ImageName
// and BaseImageLV are carried for the caller that wants to record what was baked.
type SyncImageResult struct {
	ImageName   string
	BaseImageLV string
}

// SyncImage downloads + normalizes a kernel/rootfs pair into an Atlas base image
// and imports the pristine ext4 as the read-only base LV. Ports sync-image.py's
// main(). Idempotent at two grains: a kernel already present skips the kernel
// download, and a final ext4 already present short-circuits the whole build (the
// image is complete, and the base LV import is a no-op on a re-sync anyway).
func SyncImage(ctx context.Context, cmd commands, params SyncImageParams) (SyncImageResult, error) {
	if params.GuestNetworkUnit == "" {
		params.GuestNetworkUnit = StagedGuestNetworkUnit
	}

	imageDirectory := paths.ImageDirectory(params.ImageName)
	if err := cmd.InstallDirectory(ctx, imageDirectory, "0700"); err != nil {
		return SyncImageResult{}, err
	}

	if err := downloadKernel(ctx, cmd, params, imageDirectory); err != nil {
		return SyncImageResult{}, err
	}

	// 2. Rootfs. If the final ext4 is already built, the image is complete — the
	//    isfile probe runs as root because the image dir is 0700 root-owned.
	rootfsPath := imageDirectory + "/" + params.RootfsFilename
	if cmd.OK(ctx, "sudo test -f {}", rootfsPath) {
		return SyncImageResult{ImageName: params.ImageName}, nil // "Rootfs already built. Skipping."
	}

	extracted, err := downloadRootfs(ctx, cmd, params)
	if err != nil {
		return SyncImageResult{}, err
	}
	if err := installGuestNetworkUnit(ctx, cmd, params, extracted); err != nil {
		return SyncImageResult{}, err
	}
	if err := normalizeRootfs(ctx, cmd, extracted); err != nil {
		return SyncImageResult{}, err
	}
	if err := installGuestModules(ctx, cmd, extracted, params.RootfsURL); err != nil {
		return SyncImageResult{}, err
	}
	if err := buildExt4(ctx, cmd, extracted, rootfsPath, params.DefaultDiskGB); err != nil {
		return SyncImageResult{}, err
	}

	squashfsPath := "/tmp/atlas-" + params.ImageName + ".squashfs"
	if _, err := cmd.Run(ctx, "sudo rm -rf {} {}", extracted, squashfsPath); err != nil {
		return SyncImageResult{}, err
	}

	// 5. Base image as a read-only thin LV. Per-VM disks are instant CoW snapshots
	//    of this LV; the pristine ext4 stays on disk as the import source and audit
	//    artifact. Idempotent — a no-op if the LV already exists.
	baseImageLV, err := thinpool.ImportBaseImageFromFile(ctx, cmd, params.ImageName, rootfsPath, params.DefaultDiskGB)
	if err != nil {
		return SyncImageResult{}, err
	}
	return SyncImageResult{ImageName: params.ImageName, BaseImageLV: baseImageLV}, nil
}
