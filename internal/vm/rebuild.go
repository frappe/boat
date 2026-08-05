package vm

import (
	"context"
	"errors"
	"fmt"

	"github.com/frappe/boat/internal/run"
)

// RebuildRequest names the source of the new root filesystem and what to write
// back into it.
//
// Exactly one source: a snapshot volume's device path restores the VM from its
// own snapshot, an image name lays down a base image's pristine rootfs. The
// snapshot wins when both are set.
type RebuildRequest struct {
	SnapshotDevice string
	Image          string
	// DiskGB is the size to grow the new root filesystem to — the VM's current
	// disk size, since a rebuild is not a resize.
	DiskGB int
	// FirecrackerUID is the VM's own uid; the new volume's jail node is chowned
	// back to it, or the jailed Firecracker cannot open its own disk.
	FirecrackerUID int
	Identity       Identity
	// DataSnapshotDevice restores the data disk from its own snapshot, the way
	// SnapshotDevice restores the root. Empty leaves the live data disk exactly
	// as it is, which is the safe zero value and the one a rebuild-from-image
	// takes: there is no image source for a data disk, and wiping a tenant's
	// /home because they asked to reinstall the OS is not a thing to do by
	// default.
	DataSnapshotDevice string
	DataDiskGB         int
}

// Rebuild replaces a stopped VM's root filesystem while the VM stays itself.
//
// What survives is everything that makes the VM findable: its UUID, its unit,
// its addresses, its MAC and tap, its firecracker.json, and its data disk. What
// is replaced is the root volume, which is dropped and recreated as a
// copy-on-write snapshot of the source. The identity is then written back into
// the fresh filesystem, because it came from the source's blocks and would
// otherwise be the image's identity or another VM's.
//
// The caller guarantees the VM is stopped, which is what makes swapping the
// volume under it safe and what lets the host mount it at all.
//
// Idempotent: re-running lays the same source down again.
func (manager *Manager) Rebuild(
	ctx context.Context, runner *run.Runner, uuid string, request RebuildRequest,
) error {
	commands := manager.commandsFor(runner)
	files := manager.filesFor(uuid)
	// Probed: the sentence below tells an operator to provision a VM that may be
	// provisioned already. The jail is 0700 and root-owned and this daemon is not
	// root, so "could not look" is the everyday failure here and it must not be
	// reported as a jail that is not there.
	jailed, err := hostHas(ctx, commands, "sudo test -d {}", files.jailRoot)
	if err != nil {
		return err
	}
	if !jailed {
		return fmt.Errorf("jail %s missing; provision the VM before rebuilding", files.jailRoot)
	}
	origin, err := rebuildOrigin(ctx, commands, request)
	if err != nil {
		return err
	}
	// The disk is about to change under any staged memory snapshot. Saved RAM
	// holds a filesystem cache for the OLD blocks, and restoring it over the new
	// ones is a guest that corrupts its own disk.
	if _, err := commands.Run(ctx, "sudo rm -rf {}", files.memorySnapshotDirectory); err != nil {
		return err
	}
	if err := manager.layDownRootFilesystem(ctx, commands, files, uuid, origin, request); err != nil {
		return err
	}
	return manager.restoreDataDisk(ctx, commands, files, uuid, request)
}

// rebuildOrigin resolves the volume the new root filesystem is snapshotted
// from, and refuses a source this host does not have — a snapshot that was
// never taken here, or an image that was never synced.
func rebuildOrigin(ctx context.Context, commands commands, request RebuildRequest) (volume, error) {
	if request.SnapshotDevice != "" {
		origin := volumeAtDevice(request.SnapshotDevice)
		if !origin.exists(ctx, commands) {
			return volume{}, fmt.Errorf(
				"snapshot volume %s not found (from %s)", origin.name, request.SnapshotDevice,
			)
		}
		return origin, nil
	}
	if request.Image == "" {
		return volume{}, errors.New("no rebuild source: pass an image name or a snapshot device")
	}
	origin := baseImage(request.Image)
	if !origin.exists(ctx, commands) {
		return volume{}, fmt.Errorf("base image %s not present; sync the image to this host first", origin.name)
	}
	return origin, nil
}

// layDownRootFilesystem drops the old root volume and recreates it from the
// origin, then makes it this VM's again.
//
// The remove is what forces the swap: creating the snapshot is idempotent and
// would otherwise keep the volume that is already there. The fresh ext4 UUID is
// not cosmetic either — a copy-on-write snapshot inherits the origin's, and two
// mounted filesystems claiming one UUID is a host-side blkid that lies.
func (manager *Manager) layDownRootFilesystem(
	ctx context.Context, commands commands, files virtualMachineFiles,
	uuid string, origin volume, request RebuildRequest,
) error {
	disk := rootDisk(uuid)
	if err := disk.remove(ctx, commands); err != nil {
		return err
	}
	if err := origin.snapshotInto(ctx, commands, disk); err != nil {
		return err
	}
	disk.grow(ctx, commands, request.DiskGB, true)
	if _, err := commands.Run(
		ctx, "sudo tune2fs -U random -L {} {}", rootFilesystemTag, disk.devicePath(),
	); err != nil {
		return err
	}
	if err := manager.injectIdentity(ctx, commands, disk.devicePath(), uuid, request.Identity); err != nil {
		return err
	}
	// The new volume's device number differs from the old one's, so the jail
	// node has to be re-made rather than left pointing at a device that is gone.
	return disk.exposeInJail(ctx, commands, files.rootFilesystemNode, request.FirecrackerUID)
}

// restoreDataDisk recreates the data disk from its own snapshot, in the same
// shape as the root above and for the same reasons.
//
// Nothing happens without a data snapshot to restore from. The filesystem's
// UUID is rerolled while its LABEL is kept, because the guest mounts the data
// disk by label — the fstab line the identity injection writes says
// LABEL=atlas-data, and it has to keep matching.
func (manager *Manager) restoreDataDisk(
	ctx context.Context, commands commands, files virtualMachineFiles,
	uuid string, request RebuildRequest,
) error {
	if request.DataSnapshotDevice == "" {
		return nil
	}
	origin := volumeAtDevice(request.DataSnapshotDevice)
	if !origin.exists(ctx, commands) {
		return fmt.Errorf(
			"data snapshot volume %s not found (from %s)", origin.name, request.DataSnapshotDevice,
		)
	}
	disk := dataDisk(uuid)
	if err := disk.remove(ctx, commands); err != nil {
		return err
	}
	if err := origin.snapshotInto(ctx, commands, disk); err != nil {
		return err
	}
	disk.grow(ctx, commands, request.DataDiskGB, true)
	// Unchecked, like the Python: a restored data filesystem that needs a repair
	// gets one, and e2fsck's non-zero "I fixed something" exit is not a failed
	// rebuild.
	commands.RunUnchecked(ctx, "sudo e2fsck -fy {}", disk.devicePath())
	if _, err := commands.Run(
		ctx, "sudo tune2fs -U random -L {} {}", dataFilesystemTag, disk.devicePath(),
	); err != nil {
		return err
	}
	return disk.exposeInJail(ctx, commands, files.dataNode, request.FirecrackerUID)
}
