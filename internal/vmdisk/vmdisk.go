// Package vmdisk brings a VM's disk up on the host: it re-activates the thin
// snapshot LV (lvcreate -s flags it activation-skip, so a reboot leaves it down)
// and re-mknods the block node inside the jailer chroot with the LV's current
// major:minor — the disk analogue of internal/netapply/vmnetwork, reconstructible
// from on-disk state with no database.
//
// It is the `firecracker-vm@` unit's disk ExecStartPre (`boat vm-disk-up %i`, the
// Python vm-disk-up.py's successor). Idempotent: a no-reboot restart re-activates
// an already-active LV and re-mknods the same dev_t.
//
// This is ACTIVATION and node exposure only — no lvcreate/snapshot, so it is not
// the "LVM CoW ordering" risk §3.5 warns of. It ports scripts/vm-disk-up.py and
// the activate/expose slice of scripts/lib/atlas/lvm.py. Differential-test it on a
// loop-backed thin pool (llm/TESTING.md §6a) before it cuts over.
package vmdisk

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/sidecar"
)

const (
	// volumeGroup is the atlas VG (ThinPool default). LV names are the single place
	// the scheme lives, mirrored from lvm.py:ThinPool.
	volumeGroup = "atlas"

	firecrackerUIDKey = "ATLAS_FC_UID"
)

// commands is the host-touching seam, one implementation in production, a recorder
// in tests — the same shape internal/netapply uses.
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
	OK(ctx context.Context, template string, parameters ...any) bool
}

var _ commands = (*run.Runner)(nil)

// Up activates and exposes a VM's root disk (and its data disk, when it has one).
func Up(ctx context.Context, runner *run.Runner, uuid string) error {
	if !validUUID(uuid) {
		return fmt.Errorf("vm-disk-up: %q is not a VM UUID", uuid)
	}
	virtualMachine := paths.ForVirtualMachine(uuid)
	bringUp := &bringUp{commands: runner, uuid: uuid}

	text, err := runner.Run(ctx, "sudo cat {}", virtualMachine.NetworkEnvironment())
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(sidecar.Value(text, firecrackerUIDKey))
	if err != nil {
		return fmt.Errorf("%s names no usable %s; re-provision the VM", virtualMachine.NetworkEnvironment(), firecrackerUIDKey)
	}

	// Boot-then-hydrate migration (spec/24 §0): if a dm-clone exists, the guest must
	// read THROUGH it — the plain LV is the hydration destination and is incomplete
	// until collapse. Expose the clone; otherwise activate and expose the plain LV.
	if err := bringUp.bringDiskUp(ctx, "", "atlas-vm-"+uuid, virtualMachine.RootFilesystemNode(), uid, true); err != nil {
		return err
	}
	// The data disk, when the VM has one — same activation-skip and dev_t-renumber
	// dance. A VM with no data disk (the LV is absent) is a no-op.
	return bringUp.bringDiskUp(ctx, "-data", "atlas-data-"+uuid, virtualMachine.DataNode(), uid, false)
}

type bringUp struct {
	commands commands
	uuid     string
}

// bringDiskUp exposes one disk (root or data): the dm-clone when a migration is
// booting it, else the plain LV, activated first. required=false skips a plain LV
// that does not exist (the VM has no data disk).
func (bringUp *bringUp) bringDiskUp(ctx context.Context, cloneSuffix, logicalVolume, jailNode string, uid int, required bool) error {
	if device := bringUp.cloneDevice(ctx, cloneSuffix); device != "" {
		return bringUp.exposeInJail(ctx, device, jailNode, uid)
	}
	reference := volumeGroup + "/" + logicalVolume
	if !required && !bringUp.commands.OK(ctx, "sudo lvs --noheadings {}", reference) {
		return nil
	}
	if err := bringUp.activate(ctx, reference, "/dev/"+volumeGroup+"/"+logicalVolume); err != nil {
		return err
	}
	return bringUp.exposeInJail(ctx, "/dev/"+volumeGroup+"/"+logicalVolume, jailNode, uid)
}

// cloneDevice is the dm-clone read-through device for this VM's disk during a
// boot-then-hydrate migration, or "" when there is none. suffix "-data" selects
// the data-disk clone.
func (bringUp *bringUp) cloneDevice(ctx context.Context, suffix string) string {
	name := "atlas-vm-" + bringUp.uuid + suffix + "-clone"
	if bringUp.commands.OK(ctx, "sudo dmsetup info {}", name) {
		return "/dev/mapper/" + name
	}
	return ""
}

// activate brings the LV up with -K (so an activation-skip snapshot comes up),
// waits for udev, and falls back to vgmknodes — then fails loud if the node is
// still not a block device.
func (bringUp *bringUp) activate(ctx context.Context, reference, devicePath string) error {
	if _, err := bringUp.commands.Run(ctx, "sudo lvchange -ay -K {}", reference); err != nil {
		return err
	}
	if _, err := bringUp.commands.Run(ctx, "sudo udevadm settle"); err != nil {
		return err
	}
	if bringUp.commands.OK(ctx, "test -b {}", devicePath) {
		return nil
	}
	if _, err := bringUp.commands.Run(ctx, "sudo vgmknodes {}", volumeGroup); err != nil {
		return err
	}
	if _, err := bringUp.commands.Run(ctx, "sudo udevadm settle"); err != nil {
		return err
	}
	if !bringUp.commands.OK(ctx, "test -b {}", devicePath) {
		return fmt.Errorf("LV %s activated but %s is not a block device", reference, devicePath)
	}
	return nil
}

// exposeInJail re-creates the block node inside the jailer chroot with the
// device's current major:minor, owned by the per-VM uid, 0660. Remove-and-recreate
// because the dev_t can change across a rebuild.
func (bringUp *bringUp) exposeInJail(ctx context.Context, devicePath, jailNode string, uid int) error {
	output, err := bringUp.commands.Run(ctx, "lsblk -ndo MAJ:MIN {}", devicePath)
	if err != nil {
		return err
	}
	major, minor, err := parseDeviceNumber(output)
	if err != nil {
		return fmt.Errorf("%s: %w", devicePath, err)
	}
	owner := strconv.Itoa(uid) + ":" + strconv.Itoa(uid)
	for _, step := range []struct {
		template   string
		parameters []any
	}{
		{"sudo rm -f {}", []any{jailNode}},
		{"sudo mknod {} b {} {}", []any{jailNode, major, minor}},
		{"sudo chown {} {}", []any{owner, jailNode}},
		{"sudo chmod 0660 {}", []any{jailNode}},
	} {
		if _, err := bringUp.commands.Run(ctx, step.template, step.parameters...); err != nil {
			return err
		}
	}
	return nil
}

// parseDeviceNumber reads `lsblk -ndo MAJ:MIN` output ("252:5  "), stripping the
// right-padding, into a typed major and minor so a caller can never transpose them
// or feed mknod a minor with trailing whitespace.
func parseDeviceNumber(output string) (int, int, error) {
	compact := strings.Join(strings.Fields(output), "")
	majorText, minorText, found := strings.Cut(compact, ":")
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

// validUUID admits only the 8-4-4-4-12 lowercase-hex shape, so a VM name cannot
// inject into the LV reference it becomes (atlas-vm-<uuid>).
func validUUID(uuid string) bool {
	groups := []int{8, 4, 4, 4, 12}
	parts := strings.Split(uuid, "-")
	if len(parts) != len(groups) {
		return false
	}
	for index, part := range parts {
		if len(part) != groups[index] {
			return false
		}
		for position := 0; position < len(part); position++ {
			if strings.IndexByte("0123456789abcdef", part[position]) < 0 {
				return false
			}
		}
	}
	return true
}
