package main

import (
	"context"
	"io"

	"github.com/frappe/boat/internal/image"
	"github.com/frappe/boat/internal/run"
)

// syncImage downloads a kernel and rootfs onto the host and lays the base image
// LV down — `boat sync-image`, the port of sync-image.py. It reports no result
// line, as the Python does not.
func syncImage(arguments []string, errorOutput io.Writer) int {
	flags := newTaskFlags("sync-image", errorOutput)
	name := flags.requiredText("image-name")
	kernelURL := flags.requiredText("kernel-url")
	kernelFilename := flags.requiredText("kernel-filename")
	kernelSHA256 := flags.requiredText("kernel-sha256")
	rootfsURL := flags.requiredText("rootfs-url")
	rootfsFilename := flags.requiredText("rootfs-filename")
	rootfsSHA256 := flags.requiredText("rootfs-sha256")
	diskGigabytes := flags.requiredNumber("default-disk-gb")
	// The controller stages the guest unit at a fixed path before the Task runs,
	// so the default is the always-correct path for a controller-driven sync and
	// a hand run may point it at a real file.
	guestNetworkUnit := flags.text("guest-network-unit", image.StagedGuestNetworkUnit)
	if err := flags.parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	_, err := image.SyncImage(context.Background(), run.NewRunner(errorOutput), image.SyncImageParams{
		ImageName:        *name,
		KernelURL:        *kernelURL,
		KernelFilename:   *kernelFilename,
		KernelSHA256:     *kernelSHA256,
		RootfsURL:        *rootfsURL,
		RootfsFilename:   *rootfsFilename,
		RootfsSHA256:     *rootfsSHA256,
		DefaultDiskGB:    *diskGigabytes,
		GuestNetworkUnit: *guestNetworkUnit,
	})
	if err != nil {
		return reportError(errorOutput, err)
	}
	return exitSuccess
}

// promoteSnapshotImage turns a VM's snapshot into a new base image — `boat
// promote-snapshot-image`, the port of promote-snapshot-image.py.
func promoteSnapshotImage(arguments []string, output io.Writer, errorOutput io.Writer) int {
	flags := newTaskFlags("promote-snapshot-image", errorOutput)
	device := flags.requiredText("snapshot-rootfs-path")
	name := flags.requiredText("image-name")
	diskGigabytes := flags.requiredNumber("disk-gigabytes")
	rootfsFilename := flags.requiredText("rootfs-filename")
	sourceImage := flags.requiredText("source-image")
	kernelFilename := flags.requiredText("kernel-filename")
	if err := flags.parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	result, err := image.PromoteSnapshotImage(
		context.Background(), run.NewRunner(errorOutput), image.PromoteSnapshotParams{
			SnapshotRootfsPath: *device,
			ImageName:          *name,
			DiskGigabytes:      *diskGigabytes,
			RootfsFilename:     *rootfsFilename,
			SourceImage:        *sourceImage,
			KernelFilename:     *kernelFilename,
		},
	)
	if err != nil {
		return reportError(errorOutput, err)
	}
	return emit(output, errorOutput, map[string]any{
		"image_lv":   result.ImageLV,
		"size_bytes": result.SizeBytes,
	})
}
