package vmdisk

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/frappe/boat/internal/run"
)

const thinPool = volumeGroup + "/" + poolName

// poolName is the thin pool VM disks are carved from (matches bootstrap + lvm.py).
const poolName = "pool0"

// ImportBaseImage lays a rootfs image file down as a read-only base LV, the thin
// origin every VM disk of that image is a CoW snapshot of. Creates the thin
// volume, dd's the file into it, and marks it read-only. Idempotent: an already
// imported image is left as it is.
//
// Ported from lvm.py ThinPool.import_base_image. This is an LVM operation done by
// boat rather than by hand — a base image is how one rootfs backs many VM disks
// at O(1) snapshot cost.
func ImportBaseImage(ctx context.Context, runner *run.Runner, imageName, sourceFile string, diskGigabytes int) error {
	if !validName(imageName) {
		return fmt.Errorf("image name %q is not usable", imageName)
	}
	name := "atlas-image-" + imageName
	reference := volumeGroup + "/" + name
	device := "/dev/" + volumeGroup + "/" + name
	if runner.OK(ctx, "sudo lvs --noheadings {}", reference) {
		return nil
	}
	if _, err := runner.Run(ctx, "sudo lvcreate --type thin --thinpool {} -V {} -n {} {}",
		thinPool, strconv.Itoa(diskGigabytes)+"G", name, volumeGroup); err != nil {
		return err
	}
	if _, err := runner.Run(ctx, "sudo lvchange -ay -K {}", reference); err != nil {
		return err
	}
	if _, err := runner.Run(ctx, "sudo dd if={} of={} bs=4M conv=fsync status=none", sourceFile, device); err != nil {
		// Leave no half-written base behind.
		runner.RunUnchecked(ctx, "sudo lvremove -f {}", reference)
		return err
	}
	_, err := runner.Run(ctx, "sudo lvchange --permission r {}", reference)
	return err
}

// CreateVMDisk creates a VM's root disk as a thin CoW snapshot of a base image —
// instant and O(1); the guest's writes allocate from the pool. Idempotent: an
// existing disk is re-activated, not re-created.
//
// Ported from LogicalVolume.snapshot_into. The VM disk boat vm-disk-up later
// activates and exposes in the jail.
func CreateVMDisk(ctx context.Context, runner *run.Runner, uuid, imageName string) error {
	if !validUUID(uuid) {
		return fmt.Errorf("%q is not a VM UUID", uuid)
	}
	if !validName(imageName) {
		return fmt.Errorf("image name %q is not usable", imageName)
	}
	disk := "atlas-vm-" + uuid
	reference := volumeGroup + "/" + disk
	if runner.OK(ctx, "sudo lvs --noheadings {}", reference) {
		_, err := runner.Run(ctx, "sudo lvchange -ay -K {}", reference)
		return err
	}
	if _, err := runner.Run(ctx, "sudo lvcreate -s {}/{} -n {}", volumeGroup, "atlas-image-"+imageName, disk); err != nil {
		return err
	}
	_, err := runner.Run(ctx, "sudo lvchange -ay -K {}", reference)
	return err
}

// validName accepts an LV-name-safe token (also used for interface names): the
// image name reaches an LV reference, so it may be nothing but a device-safe word.
func validName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		letter := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
		digit := character >= '0' && character <= '9'
		if !letter && !digit && !strings.ContainsRune("-_.", rune(character)) {
			return false
		}
	}
	return true
}
