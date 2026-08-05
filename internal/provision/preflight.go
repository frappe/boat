package provision

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/frappe/boat/internal/paths"
)

// preflight is steps 0 and 0b: refuse before anything is laid down.
func (provisioning *provisioning) preflight(ctx context.Context) error {
	if err := provisioning.requireImage(ctx); err != nil {
		return err
	}
	return provisioning.guardUIDCollision(ctx)
}

// requireImage verifies the image is present, and fails with an actionable
// message so the operator knows to click Sync to Server before retrying — image
// sync is multi-minute and is deliberately not auto-triggered from provision.
//
// The probe stays even when the rootfs comes from a snapshot (the clone path):
// the KERNEL is hard-linked out of the same image directory regardless of where
// the rootfs blocks come from.
//
// The Python stats the file in process; here it is `test -f` with no sudo, which
// is the same reach — /var/lib/atlas/images is root-owned and world-readable, and
// this verb runs as root.
func (provisioning *provisioning) requireImage(ctx context.Context) error {
	rootfsImage := provisioning.imageDirectory + "/" + provisioning.params.RootfsFilename
	if provisioning.commands.OK(ctx, "test -f {}", rootfsImage) {
		return nil
	}
	return fmt.Errorf(
		"image '%s' not present on server (missing %s); run Sync to Server first",
		provisioning.params.ImageName, rootfsImage,
	)
}

// guardUIDCollision fails loud if a DIFFERENT live VM's jail rootfs is already
// owned by this VM's uid.
//
// The uid is derived from the UUID and is almost always unique, but a mod
// collision is possible, and two VMs sharing a uid share the DAC identity that is
// the whole of the inter-jail isolation — each could open the other's device
// nodes. Our own node is skipped, which is what makes an idempotent re-run pass.
//
// The Python globs /var/lib/atlas/virtual-machines/*/jail/firecracker/*/root/
// rootfs.ext4 in process. Boat does not touch the host's filesystem itself, so the
// same set is enumerated by listing the VM directories and deriving each one's
// node: the jailer's --id is the VM's own UUID, so the glob's two `*`s are always
// the same value and the reconstruction is exact. `test -e` stands in for the
// glob's own existence test — a directory with no jail yet yields no path either
// way — and both reads go through sudo because the jail tree is 0700 owned by the
// per-VM uids.
func (provisioning *provisioning) guardUIDCollision(ctx context.Context) error {
	others, err := provisioning.otherVirtualMachines(ctx)
	if err != nil {
		return err
	}
	uid := strconv.Itoa(provisioning.params.FirecrackerUID)
	for _, other := range others {
		node := paths.ForVirtualMachine(other).RootFilesystemNode()
		if !provisioning.commands.OK(ctx, "sudo test -e {}", node) {
			continue
		}
		owner, err := provisioning.commands.Run(ctx, "sudo stat -c %u {}", node)
		if err != nil {
			return err
		}
		if strings.TrimSpace(owner) == uid {
			return fmt.Errorf(
				"uid %s already owned by %s; uid collision — terminate that VM or re-roll", uid, node,
			)
		}
	}
	return nil
}

// otherVirtualMachines lists every VM on this host but this one.
//
// The LISTING is read with Run, so a directory that could not be read fails the
// verb instead of reading as an empty host and waving the collision guard through.
// The `test -d` above it is the one collapse left: a probe that could not be MADE
// reads as "no VMs here", which is also what a fresh box legitimately says. That is
// safe only because this verb runs as root over SSH, where the read cannot be
// denied; if provision ever moves behind the daemon it needs run.Probe's third
// answer here, the way internal/vm's hostHas does.
//
// Non-UUID entries are dropped: the name is spliced into a path, and this is the
// one place provision learns names from the host rather than from Atlas. It is a
// hardening over the Python, which trusted its glob — the same one
// internal/reset's listVMDirectories applies.
func (provisioning *provisioning) otherVirtualMachines(ctx context.Context) ([]string, error) {
	if !provisioning.commands.OK(ctx, "test -d {}", paths.VirtualMachinesDirectory) {
		return nil, nil
	}
	listing, err := provisioning.commands.Run(ctx, "sudo ls -1 {}", paths.VirtualMachinesDirectory)
	if err != nil {
		return nil, err
	}
	var others []string
	for _, line := range strings.Split(listing, "\n") {
		name := strings.TrimSpace(line)
		if name == provisioning.params.VirtualMachineName || !paths.IsUUID(name) {
			continue
		}
		others = append(others, name)
	}
	return others, nil
}
