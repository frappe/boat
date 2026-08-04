package image

import (
	"context"
	"testing"
)

// The device, LV and path names for one promotion, spelled out so the golden reads
// like a host's journal. promote-snapshot-image.py is the oracle.
const (
	promoteSnapshotDevice = "/dev/atlas/atlas-snap-3f2504e0"
	promoteSource         = "atlas-snap-3f2504e0"
	promoteImageName      = "golden-bench-v1"
	promoteImageLV        = "atlas-image-golden-bench-v1"
	promoteImageDir       = "/var/lib/atlas/images/golden-bench-v1"
	promoteImageDevice    = "/dev/atlas/atlas-image-golden-bench-v1"
	promoteDiskGB         = 28
	promoteRootfsName     = "atlas-image-golden-bench-v1"
	promoteSourceImage    = "ubuntu-24.04"
	promoteKernelName     = "vmlinux-6.1.141"
	promoteSourceKernel   = "/var/lib/atlas/images/ubuntu-24.04/vmlinux-6.1.141"
	promoteDestKernel     = "/var/lib/atlas/images/golden-bench-v1/vmlinux-6.1.141"
	promoteSentinelPath   = promoteImageDir + "/" + promoteRootfsName
	promoteSizeBytes      = 30064771072
)

func promoteParams() PromoteSnapshotParams {
	return PromoteSnapshotParams{
		SnapshotRootfsPath: promoteSnapshotDevice,
		ImageName:          promoteImageName,
		DiskGigabytes:      promoteDiskGB,
		RootfsFilename:     promoteRootfsName,
		SourceImage:        promoteSourceImage,
		KernelFilename:     promoteKernelName,
	}
}

// newPromoteFake scripts a host where the target base image does not yet exist but
// the snapshot LV does and activates cleanly, and the source image's kernel is on
// disk to hard-link from.
func newPromoteFake() *fakeCommands {
	return newFakeCommands().
		exists("sudo lvs --noheadings atlas/"+promoteSource).
		exists("test -b "+promoteSnapshotDevice).
		exists("test -b "+promoteImageDevice).
		exists("sudo test -f "+promoteSourceKernel).
		output("sudo blockdev --getsize64 "+promoteImageDevice, "30064771072\n")
}

func TestPromoteSnapshotImageHappyPath(t *testing.T) {
	fake := newPromoteFake()

	result, err := PromoteSnapshotImage(context.Background(), fake, promoteParams())
	if err != nil {
		t.Fatalf("PromoteSnapshotImage: %v", err)
	}
	want := PromoteSnapshotResult{ImageLV: promoteImageLV, SizeBytes: promoteSizeBytes}
	if result != want {
		t.Errorf("result = %+v, want %+v", result, want)
	}

	// The full sequence: promote the snapshot LV into a read-only base image LV, make
	// the image directory, hard-link the source kernel, drop the rootfs sentinel, size
	// the LV. Order matters — the LV is materialized before the kernel is checked.
	assertTrace(t, fake,
		"? sudo lvs --noheadings atlas/"+promoteImageLV,
		"? sudo lvs --noheadings atlas/"+promoteSource,
		"sudo lvchange -ay -K atlas/"+promoteSource,
		"sudo udevadm settle",
		"? test -b "+promoteSnapshotDevice,
		"sudo lvcreate --type thin --thinpool atlas/pool0 -V 28G -n "+promoteImageLV+" atlas",
		"sudo lvchange -ay -K atlas/"+promoteImageLV,
		"sudo udevadm settle",
		"? test -b "+promoteImageDevice,
		"sudo dd if="+promoteSnapshotDevice+" of="+promoteImageDevice+" bs=4M conv=fsync status=none",
		"sudo lvchange --permission r atlas/"+promoteImageLV,
		"installdir 0700 "+promoteImageDir,
		"? sudo test -f "+promoteSourceKernel,
		"sudo ln -f "+promoteSourceKernel+" "+promoteDestKernel,
		"install 0644 "+promoteSentinelPath,
		"sudo blockdev --getsize64 "+promoteImageDevice,
	)

	// The sentinel documents that the LV, not this file, is the rootfs.
	assertInstalled(t, fake, promoteSentinelPath,
		"# Local image promoted from snapshot "+promoteSource+". Rootfs is the LVM thin "+
			"volume "+promoteImageLV+", not this file (provision reads the LV).\n")
}

// A source image whose kernel is absent fails loud: the LV is created (that runs
// first and is idempotent), but no kernel is linked and no sentinel is written.
func TestPromoteSnapshotImageRejectsMissingSourceKernel(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo lvs --noheadings atlas/" + promoteSource).
		exists("test -b " + promoteSnapshotDevice).
		exists("test -b " + promoteImageDevice)
	// The source kernel is deliberately NOT scripted present.

	if _, err := PromoteSnapshotImage(context.Background(), fake, promoteParams()); err == nil {
		t.Fatal("PromoteSnapshotImage accepted a missing source kernel")
	}
	assertIssued(t, fake, "sudo dd if="+promoteSnapshotDevice+" of="+promoteImageDevice)
	assertNotIssued(t, fake, "sudo ln -f")
	assertNotIssued(t, fake, "install 0644 "+promoteSentinelPath)
}
