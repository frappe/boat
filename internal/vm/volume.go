package vm

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Every per-VM disk is a thin LV in one volume group: a copy-on-write snapshot
// of a read-only base image LV. Resize, rebuild and terminate are the three
// verbs that reach past the unit to those volumes, and this is the only place
// in the package that knows their names.
//
// Naming is DERIVED and never stored, so a host that lost its database still
// knows which volume belongs to which VM: atlas-vm-<uuid> is the root disk,
// atlas-data-<uuid> its data peer, atlas-image-<name> a base image.
const (
	volumeGroup       = "atlas"
	thinPoolName      = "pool0"
	baseImagePrefix   = "atlas-image-"
	rootDiskPrefix    = "atlas-vm-"
	dataDiskPrefix    = "atlas-data-"
	jailNodeMode      = "0660"
	rootFilesystemTag = "atlas-root"
	dataFilesystemTag = "atlas-data"
)

// volume is one logical volume: a name, and the operations a VM verb performs
// on it.
type volume struct{ name string }

func rootDisk(uuid string) volume { return volume{name: rootDiskPrefix + uuid} }

func dataDisk(uuid string) volume { return volume{name: dataDiskPrefix + uuid} }

func baseImage(imageName string) volume { return volume{name: baseImagePrefix + imageName} }

// volumeAtDevice recovers a volume from a /dev/<group>/<name> path, which is
// how a snapshot's stored device path becomes an origin to rebuild from.
func volumeAtDevice(devicePath string) volume {
	return volume{name: devicePath[strings.LastIndex(devicePath, "/")+1:]}
}

// reference is <group>/<name>, the form the LVM tools take.
func (disk volume) reference() string { return volumeGroup + "/" + disk.name }

// devicePath is the single source of truth for where the volume lives. Callers
// never hand-build a /dev path.
func (disk volume) devicePath() string { return "/dev/" + volumeGroup + "/" + disk.name }

// protected marks the volumes a per-VM verb must never destroy: the thin pool
// itself, and the base images every VM's disk is a snapshot OF. A bug that
// passed the wrong name to a remove can then take at most one VM's own disk,
// rather than the shared state every VM on the host depends on.
func (disk volume) protected() bool {
	return disk.name == thinPoolName || strings.HasPrefix(disk.name, baseImagePrefix)
}

// exists asks LVM whether the volume is there.
//
// Left as OK, and not because the collapse is free — three of its callers report
// the answer in a sentence. It is because lvs cannot be probed: it EXPLAINS its
// negative, `Failed to find logical volume "atlas/…"` on stderr with exit 5,
// which is the same shape as a denial and is the case run.Probe names as
// unaskable. The honest form is a LISTING — one `lvs` over the volume group that
// exits zero and answers with its output, the way internal/adopt reads the
// volume group itself — and that is a new command line, so it is a new sudoers
// grant and belongs with one.
func (disk volume) exists(ctx context.Context, commands commands) bool {
	return commands.OK(ctx, "sudo lvs --noheadings {}", disk.reference())
}

// remove drops the volume, and is a no-op when it is already gone — which is
// what makes a re-run of terminate finish a job an earlier attempt left half
// done.
func (disk volume) remove(ctx context.Context, commands commands) error {
	if disk.protected() {
		return fmt.Errorf("refusing to remove protected volume %s", disk.name)
	}
	if !disk.exists(ctx, commands) {
		return nil
	}
	_, err := commands.Run(ctx, "sudo lvremove -f {}", disk.reference())
	return err
}

func (disk volume) sizeBytes(ctx context.Context, commands commands) (int64, error) {
	output, err := commands.Run(ctx, "sudo blockdev --getsize64 {}", disk.devicePath())
	if err != nil {
		return 0, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(output), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size of %s: %w", disk.devicePath(), err)
	}
	return size, nil
}

// activate brings the volume up and waits for its device node to appear.
//
// -K is not optional: a thin snapshot carries the activation-skip flag, and
// every per-VM disk is a thin snapshot, so a plain lvchange -ay leaves it down.
// The settle-then-vgmknodes dance is there because the node is created by udev
// asynchronously; without it the next command opens a path that does not exist
// yet, which on a real host is a rebuild that fails one time in some.
func (disk volume) activate(ctx context.Context, commands commands) error {
	if _, err := commands.Run(ctx, "sudo lvchange -ay -K {}", disk.reference()); err != nil {
		return err
	}
	if _, err := commands.Run(ctx, "sudo udevadm settle"); err != nil {
		return err
	}
	node, err := disk.nodeIsBlockDevice(ctx, commands)
	if err != nil || node {
		return err
	}
	if _, err := commands.Run(ctx, "sudo vgmknodes {}", volumeGroup); err != nil {
		return err
	}
	if _, err := commands.Run(ctx, "sudo udevadm settle"); err != nil {
		return err
	}
	node, err = disk.nodeIsBlockDevice(ctx, commands)
	if err != nil {
		return err
	}
	if !node {
		return fmt.Errorf("%s activated but %s is not a block device", disk.name, disk.devicePath())
	}
	return nil
}

// nodeIsBlockDevice asks the host rather than stat-ing in process, which is the
// one difference from the Python: this daemon does not touch the filesystem
// itself, so that a test of a verb needs no host at all.
//
// Probed rather than OK'd because both callers act on it: the first skips the
// vgmknodes dance, the second REPORTS "activated but … is not a block device",
// which is a claim about udev. test(1) has a silent negative, so the third
// answer here is a `test` that could not be run at all, and saying so beats
// blaming the device node.
func (disk volume) nodeIsBlockDevice(ctx context.Context, commands commands) (bool, error) {
	return hostHas(ctx, commands, "test -b {}", disk.devicePath())
}

// snapshotInto creates target as a copy-on-write thin snapshot of this volume
// and activates it. Instant and O(1), whatever the size — the bytes are shared
// with the origin until something writes.
//
// Idempotent: a target that already exists is re-activated rather than
// recreated, which is why a rebuild REMOVES the old disk first. The remove is
// what forces the swap; this alone would keep the old contents.
func (disk volume) snapshotInto(ctx context.Context, commands commands, target volume) error {
	if target.exists(ctx, commands) {
		return target.activate(ctx, commands)
	}
	// No -L and no --thinpool: snapshotting a thin LV inherits both from its
	// origin.
	if _, err := commands.Run(ctx, "sudo lvcreate -s {} -n {}", disk.reference(), target.name); err != nil {
		return err
	}
	return target.activate(ctx, commands)
}

// grow extends the volume to gigabytes, and the filesystem on it when
// withFilesystem — `lvextend -r` does both in one shot. No size means no grow:
// the volume keeps whatever it inherited.
//
// The exit code is discarded, as it is in the Python: lvextend REFUSES to
// shrink and exits non-zero when the volume already meets the size, and both of
// those are the correct outcome for a re-run. The refusal is not what protects
// the data, though — see refuseToShrink, which is what makes a shrink a
// declined request instead of a swallowed error.
func (disk volume) grow(ctx context.Context, commands commands, gigabytes int, withFilesystem bool) {
	if gigabytes <= 0 {
		return
	}
	template := "sudo lvextend -L {} {}"
	if withFilesystem {
		template = "sudo lvextend -r -L {} {}"
	}
	commands.RunUnchecked(ctx, template, fmt.Sprintf("%dG", gigabytes), disk.devicePath())
}

// exposeInJail re-creates the block-special node the jailed Firecracker opens.
//
// Always remove and re-create, never reuse: a rebuild gives the VM a new
// volume, whose device number differs from the old one, so a surviving node
// would point at a device that is gone. Access inside the jail is pure DAC,
// which is why the node is chowned to the VM's own uid and left 0660.
func (disk volume) exposeInJail(
	ctx context.Context, commands commands, jailNode string, firecrackerUID int,
) error {
	output, err := commands.Run(ctx, "lsblk -ndo MAJ:MIN {}", disk.devicePath())
	if err != nil {
		return err
	}
	major, minor, err := deviceNumber(output)
	if err != nil {
		return err
	}
	if _, err := commands.Run(ctx, "sudo rm -f {}", jailNode); err != nil {
		return err
	}
	if _, err := commands.Run(ctx, "sudo mknod {} b {} {}", jailNode, major, minor); err != nil {
		return err
	}
	owner := fmt.Sprintf("%d:%d", firecrackerUID, firecrackerUID)
	if _, err := commands.Run(ctx, "sudo chown {} {}", owner, jailNode); err != nil {
		return err
	}
	_, err = commands.Run(ctx, "sudo chmod {} {}", jailNodeMode, jailNode)
	return err
}

// deviceNumber parses `lsblk -ndo MAJ:MIN`, whose column is right-padded
// ("252:5  "). Stripping every space before the split is deliberate: a minor
// number carrying trailing whitespace into mknod is a bug this codebase has
// already paid for once.
func deviceNumber(output string) (major int, minor int, err error) {
	digits := strings.Join(strings.Fields(output), "")
	majorText, minorText, found := strings.Cut(digits, ":")
	if !found {
		return 0, 0, fmt.Errorf("lsblk reported no MAJ:MIN: %q", output)
	}
	if major, err = strconv.Atoi(majorText); err != nil {
		return 0, 0, fmt.Errorf("device major in %q: %w", output, err)
	}
	if minor, err = strconv.Atoi(minorText); err != nil {
		return 0, 0, fmt.Errorf("device minor in %q: %w", output, err)
	}
	return major, minor, nil
}
