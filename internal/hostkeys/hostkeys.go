// Package hostkeys rotates a Stopped VM's SSH host keys on demand: it mounts the
// VM's root LV on the host, replaces /etc/ssh/ssh_host_* with fresh per-VM keys,
// and unmounts. The VM stays Stopped; the new keys take effect on the next Start.
// Clients then see a changed host key and must refresh known_hosts — which is the
// whole point of asking for a rotation.
//
// This is the explicit, opt-in counterpart to the preserve-by-default identity
// injection Boat does on rebuild/restore (internal/vm InjectIdentity, which keeps
// whatever keys the disk carries). Provision establishes host keys at birth,
// rebuild/restore PRESERVE them, and this is the ONE verb that deliberately
// changes them. Because it changes SSH identity, it is never a side effect of
// anything else.
//
// Ported from scripts/regenerate-host-keys-vm.py and the
// regenerate_host_keys_on_device / _regenerate_host_keys slice of
// scripts/lib/atlas/rootfs.py, plus the activate slice of scripts/lib/atlas/lvm.py.
// Everything here is a sequence of commands against the host, driven through the
// one `commands` seam, so a differential test can compare it to the Python with no
// LVM stack, no mount and no root.
package hostkeys

import (
	"context"
	"fmt"
	"strings"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/run"
)

// volumeGroup is the atlas VG (ThinPool default in lvm.py). The LV-name scheme —
// atlas-vm-<uuid> for a root disk — is mirrored from lvm.py:ThinPool.vm_disk, the
// single place that scheme lives.
const volumeGroup = "atlas"

// commands is everything this package does to the host, and the only seam it has.
// Outside tests there is one implementation, *run.Runner; a test drives a recorder
// so the exact command sequence is what a differential comparison against the
// Python checks.
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
	RunUnchecked(ctx context.Context, template string, parameters ...any) (string, error)
	OK(ctx context.Context, template string, parameters ...any) bool
	InstallDirectory(ctx context.Context, destination string, mode string) error
}

var _ commands = (*run.Runner)(nil)

// RegenerateHostKeysParams names the VM whose SSH host keys to rotate. The UUID
// locates the root disk LV (atlas-vm-<uuid>) and seeds the key comment
// (atlas-<uuid8>). The parent threads it from the API/CLI layer.
type RegenerateHostKeysParams struct {
	VirtualMachine string // the VM UUID
}

// RegenerateHostKeysResult reports the rotation for the operation record. The
// Python emits no ATLAS_RESULT line (a rotation just rotates), so this is a small
// echo the controller can log rather than a parsed contract.
type RegenerateHostKeysResult struct {
	VirtualMachine string
	Hostname       string // the per-VM key comment written into the fresh key (atlas-<uuid8>)
}

// RegenerateHostKeysVM rotates a Stopped VM's SSH host keys. The caller guarantees
// the VM is Stopped — the rootfs is unmounted in the guest, so mounting it on the
// host is safe. Idempotent in the sense the whole verb is: a re-run just generates
// a newer key.
func RegenerateHostKeysVM(
	ctx context.Context, cmd commands, params RegenerateHostKeysParams,
) (RegenerateHostKeysResult, error) {
	// The UUID becomes an LV reference and path segments, both matched by a `*` in
	// the sudoers allow-list, so it must be a real UUID before it is rendered
	// anywhere — the same guard vmdisk and the adoption scan make on host-learned
	// names (see paths.IsUUID).
	if !paths.IsUUID(params.VirtualMachine) {
		return RegenerateHostKeysResult{}, fmt.Errorf(
			"regenerate-host-keys-vm: %q is not a VM UUID", params.VirtualMachine,
		)
	}
	reference := volumeGroup + "/atlas-vm-" + params.VirtualMachine
	device := "/dev/" + volumeGroup + "/atlas-vm-" + params.VirtualMachine

	// The disk LV must exist — the Python's `disk.exists` gate. A missing LV means
	// the VM was never provisioned, and there is nothing to rotate.
	if !cmd.OK(ctx, "sudo lvs --noheadings {}", reference) {
		return RegenerateHostKeysResult{}, fmt.Errorf(
			"disk LV atlas-vm-%s missing; provision the VM first", params.VirtualMachine,
		)
	}

	virtualMachine := paths.ForVirtualMachine(params.VirtualMachine)
	// The rootfs is about to change under any pending memory snapshot; saved RAM
	// referencing the old disk must never be restored over the new one, so its
	// directory goes first.
	if _, err := cmd.Run(ctx, "sudo rm -rf {}", virtualMachine.MemorySnapshotDirectory()); err != nil {
		return RegenerateHostKeysResult{}, err
	}

	// Activate (idempotent) before mounting — a Stopped VM's thin snapshot LV may be
	// deactivated (lvcreate -s flags it activation-skip).
	if err := activate(ctx, cmd, reference, device); err != nil {
		return RegenerateHostKeysResult{}, err
	}

	// The key comment mirrors the per-VM hostname (atlas-<uuid8>), the same value
	// identity injection uses; it is cosmetic (the `-C` on the key).
	hostname := hostnameFor(params.VirtualMachine)
	if err := regenerateHostKeysOnDevice(ctx, cmd, device, hostname); err != nil {
		return RegenerateHostKeysResult{}, err
	}
	return RegenerateHostKeysResult{VirtualMachine: params.VirtualMachine, Hostname: hostname}, nil
}

// activate brings the LV up with -K (so an activation-skip snapshot comes up),
// waits for udev, and falls back to vgmknodes — then fails loud if the node is
// still not a block device. The port of LogicalVolume.activate / _wait_for_node;
// the os.stat block-device check the Python makes renders as `test -b` here, the
// same translation internal/vmdisk uses.
func activate(ctx context.Context, cmd commands, reference, device string) error {
	if _, err := cmd.Run(ctx, "sudo lvchange -ay -K {}", reference); err != nil {
		return err
	}
	if _, err := cmd.Run(ctx, "sudo udevadm settle"); err != nil {
		return err
	}
	if cmd.OK(ctx, "test -b {}", device) {
		return nil
	}
	if _, err := cmd.Run(ctx, "sudo vgmknodes {}", volumeGroup); err != nil {
		return err
	}
	if _, err := cmd.Run(ctx, "sudo udevadm settle"); err != nil {
		return err
	}
	if !cmd.OK(ctx, "test -b {}", device) {
		return fmt.Errorf("LV %s activated but %s is not a block device", reference, device)
	}
	return nil
}

// regenerateHostKeysOnDevice mounts the device on a fresh temp dir, replaces its
// SSH host keys, and unmounts — on success and on failure both. A mount left
// behind holds the volume open, and the next thing that happens to it is a Start
// handing it to a jailed Firecracker. Ports rootfs.py:regenerate_host_keys_on_device
// and its _mounted context manager (the shell `trap ... EXIT` the Python replaced).
func regenerateHostKeysOnDevice(ctx context.Context, cmd commands, device, hostname string) error {
	output, err := cmd.Run(ctx, "sudo mktemp -d /tmp/atlas-mount-XXXXXX")
	if err != nil {
		return err
	}
	mountPoint := strings.TrimSpace(output)
	// The LV is a block device — mount it directly, no `-o loop`.
	if _, err := cmd.Run(ctx, "sudo mount {} {}", device, mountPoint); err != nil {
		return err
	}
	defer unmount(ctx, cmd, mountPoint)
	return regenerateHostKeys(ctx, cmd, mountPoint, hostname)
}

// unmount is cleanup, so it is unchecked end to end: reporting a failure here would
// fail a rotation whose work succeeded, and the rmdir is expected to fail whenever
// the umount did.
func unmount(ctx context.Context, cmd commands, mountPoint string) {
	cmd.RunUnchecked(ctx, "sudo umount {}", mountPoint)
	cmd.RunUnchecked(ctx, "sudo rmdir {}", mountPoint)
}

// regenerateHostKeys replaces the rootfs's SSH host keys with fresh per-VM ones.
//
// Only ed25519 is generated: it is the fastest to create (~0.03s against ~0.9s for
// RSA) and every modern client negotiates it first. Any inherited rsa/ecdsa keys
// are deleted anyway — a snapshot/clone source may carry them, and keeping them
// would let the new VM answer with the source VM's identity. Ports
// rootfs.py:_regenerate_host_keys.
func regenerateHostKeys(ctx context.Context, cmd commands, mountPoint, hostname string) error {
	if err := cmd.InstallDirectory(ctx, mountPoint+"/etc/ssh", "0755"); err != nil {
		return err
	}
	for _, algorithm := range []string{"rsa", "ecdsa"} {
		stale := mountPoint + "/etc/ssh/ssh_host_" + algorithm + "_key"
		if _, err := cmd.Run(ctx, "sudo rm -f {} {}", stale, stale+".pub"); err != nil {
			return err
		}
	}
	key := mountPoint + "/etc/ssh/ssh_host_ed25519_key"
	if _, err := cmd.Run(ctx, "sudo rm -f {} {}", key, key+".pub"); err != nil {
		return err
	}
	_, err := cmd.Run(ctx, "sudo ssh-keygen -q -t ed25519 -f {} -N {} -C {}", key, "", "root@"+hostname)
	return err
}

// hostnameFor is the first eight characters of the UUID — enough to recognise the
// VM in a journal line, and the same value identity injection derives.
func hostnameFor(uuid string) string {
	if len(uuid) > 8 {
		uuid = uuid[:8]
	}
	return "atlas-" + uuid
}
