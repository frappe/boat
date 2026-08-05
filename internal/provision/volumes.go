package provision

import (
	"context"
	"fmt"
	"path"
	"strconv"

	"github.com/frappe/boat/internal/thinpool"
)

// layDownVolumes is steps 1 and 1b: the per-VM root disk, and its data peer when
// the VM has one.
//
// Boot-on-clone only if the clone device is BOTH requested AND actually present on
// the host. A collapse-forward retry — or any re-provision — may pass the clone
// path after the disk has already converged to the plain LV (the clone is removed
// at stop), and falling back to an ordinary plain-LV provision then is right;
// failing on a missing clone would strand the retry.
func (provisioning *provisioning) layDownVolumes(ctx context.Context, markerWasPending bool) error {
	provisioning.bootOnClone = provisioning.params.CloneRootfsDevice != "" &&
		provisioning.commands.OK(ctx, "sudo dmsetup info {}", path.Base(provisioning.params.CloneRootfsDevice))
	if err := provisioning.layDownRootVolume(ctx, markerWasPending); err != nil {
		return err
	}
	if provisioning.params.DataDiskGB <= 0 {
		return nil
	}
	return provisioning.layDownDataVolume(ctx)
}

// layDownRootVolume brings the VM's root disk into being.
//
// Normally an instant CoW thin snapshot of an origin LV — the pristine image's
// base LV, or a snapshot LV when cloning. No full copy: unwritten blocks stay
// shared with the origin. The identity injected in step 2 is freshly derived from
// THIS VM's UUID, so a clone never shares host keys or machine-id with its source.
//
// A WARM clone's disk must stay a byte-exact CoW of the golden: the frozen RAM's
// filesystem cache references exactly those blocks, so ANY offline mutation — a
// grow, a tune2fs UUID reroll, an identity injection — would corrupt the resumed
// guest. Bare snapshot only.
//
// A BOOT-ON-CLONE disk is the dm-clone's dest LV, already created by clone-target
// and hydrating live. Do NOT snapshot or grow it: that would race the read-through
// and corrupt the copy. The clone device is exposed as the jail rootfs node later.
func (provisioning *provisioning) layDownRootVolume(ctx context.Context, markerWasPending bool) error {
	if provisioning.bootOnClone {
		if !thinpool.Exists(ctx, provisioning.commands, provisioning.rootVolume) {
			return fmt.Errorf(
				"boot-on-clone requested but dest LV %s does not exist; "+
					"run migration-clone-target first (spec/24 §0)", provisioning.rootVolume,
			)
		}
		return nil
	}
	origin, err := provisioning.resolveOrigin(ctx)
	if err != nil {
		return err
	}
	if !provisioning.warm {
		return prepareRootVolume(ctx, provisioning.commands, origin, provisioning.rootVolume, provisioning.params.DiskGB)
	}
	// The warm pair is staged only when THIS run created the disk, or when a still
	// present marker proves the previous staging was never consumed (the guest never
	// ran). RAM must never be restored over a disk that has since diverged from it.
	provisioning.stageWarm = !thinpool.Exists(ctx, provisioning.commands, provisioning.rootVolume) || markerWasPending
	return thinpool.SnapshotInto(ctx, provisioning.commands, origin, provisioning.rootVolume)
}

// layDownDataVolume brings the optional data disk (the guest's /dev/vdb) into
// being — the root disk's peer, a blank thin volume normally or a CoW snapshot of
// a data-disk snapshot LV when cloning.
//
// It is built BEFORE identity injection so its `atlas-data` ext4 label exists by
// the time the LABEL=atlas-data fstab line is written into the root rootfs.
//
// Boot-on-clone: the data disk is its own dm-clone (the root clone's name with
// `-clone` → `-data-clone`) hydrating live, exactly like root. Its dest LV already
// exists; preparing it would race the read-through.
func (provisioning *provisioning) layDownDataVolume(ctx context.Context) error {
	if provisioning.bootOnClone {
		if !thinpool.Exists(ctx, provisioning.commands, provisioning.dataVolume) {
			return fmt.Errorf(
				"boot-on-clone requested but data dest LV %s does not exist; "+
					"run migration-clone-target first (spec/24 §0)", provisioning.dataVolume,
			)
		}
		return nil
	}
	origin := ""
	if provisioning.params.DataSnapshotRootfsPath != "" {
		origin = thinpool.NameFromDevice(provisioning.params.DataSnapshotRootfsPath)
	}
	return prepareDataVolume(
		ctx, provisioning.commands, provisioning.dataVolume,
		provisioning.params.DataDiskGB, provisioning.params.DataDiskFormat != 0, origin,
	)
}

// resolveOrigin resolves the LV the per-VM disk snapshots from: a snapshot LV wins
// (the clone path), otherwise the base image LV. Both are refused when absent,
// with the message that names the fix — the same guards rebuild makes.
func (provisioning *provisioning) resolveOrigin(ctx context.Context) (string, error) {
	if provisioning.params.SnapshotRootfsPath != "" {
		origin := thinpool.NameFromDevice(provisioning.params.SnapshotRootfsPath)
		if !thinpool.Exists(ctx, provisioning.commands, origin) {
			return "", fmt.Errorf(
				"snapshot LV not found: %s (from %s)", origin, provisioning.params.SnapshotRootfsPath,
			)
		}
		return origin, nil
	}
	origin := thinpool.BaseImageLV(provisioning.params.ImageName)
	if !thinpool.Exists(ctx, provisioning.commands, origin) {
		return "", fmt.Errorf("base image LV not found: %s; run Sync to Server first", origin)
	}
	return origin, nil
}

// prepareRootVolume creates target as a CoW thin snapshot of origin, grows it to
// gigabytes if larger, gives it a fresh ext4 UUID and label, and leaves it
// activated. Ports rootfs.py:prepare_lv. Idempotent: the snapshot no-ops (and
// re-activates) when target already exists, so a re-provision reuses the disk.
func prepareRootVolume(ctx context.Context, cmd commands, origin, target string, gigabytes int) error {
	if err := thinpool.SnapshotInto(ctx, cmd, origin, target); err != nil {
		return err
	}
	device := thinpool.DevicePath(target)
	grow(ctx, cmd, "sudo lvextend -r -L {} {}", gigabytes, device)
	// A CoW snapshot inherits the origin's ext4 UUID; a fresh one per VM keeps
	// host-side blkid honest. The guest mounts root=/dev/vda and is UUID-agnostic,
	// and this happens while the volume is unmounted.
	_, err := cmd.Run(ctx, "sudo tune2fs -U random -L atlas-root {}", device)
	return err
}

// prepareDataVolume brings a per-VM data disk into being and leaves it activated —
// the data-disk peer of prepareRootVolume, with two sources. Ports
// rootfs.py:prepare_data_lv. Idempotent: the create/snapshot re-activates an
// existing LV and the grow/e2fsck/tune2fs steps are no-ops once satisfied, so a
// re-provision reuses the same disk WITHOUT wiping it.
func prepareDataVolume(
	ctx context.Context, cmd commands, target string, gigabytes int, format bool, origin string,
) error {
	if origin != "" {
		return cloneDataVolume(ctx, cmd, target, gigabytes, origin)
	}
	return blankDataVolume(ctx, cmd, target, gigabytes, format)
}

// cloneDataVolume is the clone/restore source: a CoW thin snapshot of a data-disk
// snapshot LV, exactly as the root is. The UUID is rerolled while the `atlas-data`
// LABEL is KEPT, because the guest mounts by LABEL=atlas-data. No mkfs — the data
// is what is being preserved.
func cloneDataVolume(ctx context.Context, cmd commands, target string, gigabytes int, origin string) error {
	if err := thinpool.SnapshotInto(ctx, cmd, origin, target); err != nil {
		return err
	}
	device := thinpool.DevicePath(target)
	grow(ctx, cmd, "sudo lvextend -r -L {} {}", gigabytes, device)
	// Unchecked like the Python: a restored data filesystem that needs a repair gets
	// one, and e2fsck's non-zero "I fixed something" exit is not a failed provision.
	cmd.RunUnchecked(ctx, "sudo e2fsck -fy {}", device)
	_, err := cmd.Run(ctx, "sudo tune2fs -U random -L atlas-data {}", device)
	return err
}

// blankDataVolume is the fresh disk: a private thin volume with no origin. It is
// formatted the FIRST time only — a freshly minted LV — because mkfs on a later
// run would wipe the tenant's data; a later run grows the LV and its filesystem
// instead. An unformatted (raw) disk is attached as a block device and grown with
// a plain lvextend, since there is no filesystem to -r.
//
// thinpool.CreateThin renders `--thinpool atlas/pool0` where lvm.py renders the
// bare `pool0`. That is the one command in this verb that is not byte-identical to
// the Python's; both are accepted by lvcreate (the VG operand disambiguates), and
// the qualified form is what internal/vmdisk and internal/bootstrap have created
// volumes with on live hosts.
func blankDataVolume(ctx context.Context, cmd commands, target string, gigabytes int, format bool) error {
	freshlyCreated := !thinpool.Exists(ctx, cmd, target)
	if err := thinpool.CreateThin(ctx, cmd, target, gigabytes); err != nil {
		return err
	}
	device := thinpool.DevicePath(target)
	switch {
	case format && freshlyCreated:
		// -F: non-interactive even though the device is whole-disk (no partition).
		_, err := cmd.Run(ctx, "sudo mkfs.ext4 -q -L atlas-data -F {}", device)
		return err
	case format:
		grow(ctx, cmd, "sudo lvextend -r -L {} {}", gigabytes, device)
		cmd.RunUnchecked(ctx, "sudo e2fsck -fy {}", device)
	case !freshlyCreated:
		grow(ctx, cmd, "sudo lvextend -L {} {}", gigabytes, device)
	}
	return nil
}

// grow extends a volume, and its exit code is deliberately discarded: lvextend
// REFUSES to shrink and exits non-zero when the volume already meets the size,
// which is the correct outcome for the re-provision this whole verb is built to
// survive. The backstop is the command that follows every call — a tune2fs, an
// e2fsck or a mkfs against the same device — which fails loudly if the volume is
// not actually there.
func grow(ctx context.Context, cmd commands, template string, gigabytes int, device string) {
	cmd.RunUnchecked(ctx, template, strconv.Itoa(gigabytes)+"G", device)
}
