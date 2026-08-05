package provision

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/frappe/boat/internal/thinpool"
)

// freshJailTree creates the VM's directories and clears any staged memory
// snapshot, and reports whether that snapshot's READY marker was there.
//
// Every directory is 0700 and explicit: a jail left at the umask's mercy is a jail
// another VM's uid can read.
//
// A (re)provision lays a fresh disk, and a leftover memory snapshot would pair
// stale RAM with it, so it is dropped and the next start cold-boots. For a warm
// clone a still-present marker means the opposite: it proves the staged pair was
// never consumed (the guest never ran), which is what makes re-staging safe on an
// idempotent re-run.
func (provisioning *provisioning) freshJailTree(ctx context.Context) (bool, error) {
	for _, directory := range []string{
		provisioning.virtualMachine.Directory(),
		provisioning.virtualMachine.LogDirectory(),
		provisioning.virtualMachine.JailRoot(),
		provisioning.virtualMachine.APISocketDirectory(),
	} {
		if err := provisioning.commands.InstallDirectory(ctx, directory, "0700"); err != nil {
			return false, err
		}
	}
	if provisioning.warm && provisioning.params.DataDiskGB > 0 {
		return false, errors.New("a warm clone cannot carry a data disk; the golden was captured without one")
	}
	markerWasPending := provisioning.commands.OK(
		ctx, "sudo test -f {}", provisioning.virtualMachine.MemorySnapshotMarker(),
	)
	_, err := provisioning.commands.Run(
		ctx, "sudo rm -rf {}", provisioning.virtualMachine.MemorySnapshotDirectory(),
	)
	return markerWasPending, err
}

// buildJail is steps 3 through 5: the kernel, the Firecracker config, the block
// nodes, a warm clone's MMDS payload, and then the whole tree handed to the per-VM
// uid.
//
// The kernel is HARD-LINKED, not copied, so the immutable image kernel is not
// duplicated per VM; it is the same filesystem (/var/lib/atlas), so the link always
// succeeds, and read-only is all the jailed process needs.
//
// The recursive chown is last, after every file is in place. The jailer chowns the
// jail root and the device nodes it creates itself, but the backing files laid down
// here — the kernel link and the config — must be uid-owned too. It re-touches the
// rootfs.ext4 node's inode, which is correct and harmless: it chowns the NODE, not
// the LV the node points at.
func (provisioning *provisioning) buildJail(ctx context.Context) error {
	kernel := provisioning.imageDirectory + "/" + provisioning.params.KernelFilename
	if _, err := provisioning.commands.Run(
		ctx, "sudo ln -f {} {}", kernel, provisioning.virtualMachine.Kernel(),
	); err != nil {
		return err
	}
	configuration, err := firecrackerConfiguration(provisioning.params)
	if err != nil {
		return err
	}
	if err := provisioning.commands.InstallFile(
		ctx, configuration, provisioning.virtualMachine.FirecrackerConfig(), "0644",
	); err != nil {
		return err
	}
	if err := provisioning.exposeDisks(ctx); err != nil {
		return err
	}
	if err := provisioning.stageMetadata(ctx); err != nil {
		return err
	}
	_, err = provisioning.commands.Run(
		ctx, "sudo chown -R {} {}", provisioning.owner(), provisioning.virtualMachine.JailChrootBase(),
	)
	return err
}

// exposeDisks is steps 4b and 4c: the root disk as a block-special node at
// rootfs.ext4, and the data disk at data.ext4 when the VM has one.
//
// firecracker.json names both jail-RELATIVE (`path_on_host: "rootfs.ext4"`), which
// resolves to these nodes after the chroot — Firecracker opens each as a plain
// block device, no config change from the file-backed era.
//
// Boot-on-clone (spec/24 §0) points each node at the CLONE device instead, so
// Firecracker reads THROUGH the clone and hydration serves un-copied blocks from
// the source over NBD. At CollapseClone the clone's table is reloaded to a linear
// map onto the same dest LV, keeping the same dm major:minor — so this node and
// Firecracker's open fd both stay valid. The data disk has its own dm-clone, named
// by replacing the root clone's `-clone` with `-data-clone`.
func (provisioning *provisioning) exposeDisks(ctx context.Context) error {
	rootDevice := thinpool.DevicePath(provisioning.rootVolume)
	if provisioning.bootOnClone {
		rootDevice = provisioning.params.CloneRootfsDevice
	}
	if err := exposeDeviceInJail(
		ctx, provisioning.commands, rootDevice,
		provisioning.virtualMachine.RootFilesystemNode(), provisioning.owner(),
	); err != nil {
		return err
	}
	if provisioning.params.DataDiskGB <= 0 {
		return nil
	}
	dataDevice := thinpool.DevicePath(provisioning.dataVolume)
	if provisioning.bootOnClone {
		dataDevice = strings.ReplaceAll(provisioning.params.CloneRootfsDevice, "-clone", "-data-clone")
	}
	return exposeDeviceInJail(
		ctx, provisioning.commands, dataDevice, provisioning.virtualMachine.DataNode(), provisioning.owner(),
	)
}

// stageMetadata is step 4d: a warm clone's identity, staged as the MMDS payload.
//
// The guest cannot learn its identity from the disk (the identity write was
// skipped), so the freshen unit baked into the golden reads it from the metadata
// service at 169.254.169.254 — the restore verb PUTs this file into MMDS before
// resuming, and the launcher preloads it with --metadata on a cold boot.
func (provisioning *provisioning) stageMetadata(ctx context.Context) error {
	if !provisioning.warm {
		return nil
	}
	payload, err := mmdsMetadata(provisioning.params)
	if err != nil {
		return err
	}
	return provisioning.commands.InstallFile(
		ctx, payload, provisioning.virtualMachine.MetadataFile(), "0644",
	)
}

// exposeDeviceInJail exposes a block device inside the jailer chroot at jailNode,
// owned by the per-VM uid (gid == uid), mode 0660.
//
// The device may be an LVM LV or a device-mapper node — the mknod machinery is
// identical, it needs only the device's major:minor. Always remove-and-recreate,
// because the dev_t can change across a rebuild. Access inside the jail is pure
// DAC, which is why the node is chowned to the VM's own uid. Ports
// lvm.py:expose_device_in_jail.
func exposeDeviceInJail(ctx context.Context, cmd commands, devicePath, jailNode, owner string) error {
	output, err := cmd.Run(ctx, "lsblk -ndo MAJ:MIN {}", devicePath)
	if err != nil {
		return err
	}
	major, minor, err := deviceNumber(output)
	if err != nil {
		return fmt.Errorf("%s: %w", devicePath, err)
	}
	if _, err := cmd.Run(ctx, "sudo rm -f {}", jailNode); err != nil {
		return err
	}
	if _, err := cmd.Run(ctx, "sudo mknod {} b {} {}", jailNode, major, minor); err != nil {
		return err
	}
	if _, err := cmd.Run(ctx, "sudo chown {} {}", owner, jailNode); err != nil {
		return err
	}
	_, err = cmd.Run(ctx, "sudo chmod 0660 {}", jailNode)
	return err
}

// deviceNumber parses `lsblk -ndo MAJ:MIN`, whose column is right-padded
// ("252:5  "). Every space is stripped before the split because a minor number
// carrying trailing whitespace into mknod is a bug this codebase has paid for once
// (lvm.py's DeviceNumber.from_lsblk records the same).
func deviceNumber(output string) (int, int, error) {
	digits := strings.Join(strings.Fields(output), "")
	majorText, minorText, found := strings.Cut(digits, ":")
	if !found {
		return 0, 0, fmt.Errorf("lsblk gave no MAJ:MIN: %q", output)
	}
	major, majorErr := strconv.Atoi(majorText)
	minor, minorErr := strconv.Atoi(minorText)
	if majorErr != nil || minorErr != nil {
		return 0, 0, fmt.Errorf("lsblk MAJ:MIN not numbers: %q", output)
	}
	return major, minor, nil
}
