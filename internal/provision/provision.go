// Package provision creates one Firecracker VM on this host and starts it: the
// per-VM root disk (and its optional data peer), this VM's identity written into
// that disk, the jail the Firecracker process is confined to, the sidecar the
// network hook reads back after a reboot, the launcher the systemd unit execs,
// and finally the unit itself.
//
// It is the port of scripts/provision-vm.py, the largest of Atlas's host verbs,
// and it is a port rather than a design. Four generated files leave this package
// — firecracker.json, metadata.json, network.env and jailer-launch.sh — and each
// is held to the Python's bytes, because a live host carries VMs provisioned by
// both and because two other verbs read the launcher's TEXT back: internal/vm's
// resize splices its `--cgroup` lines, and its sleep probe greps it for
// snapshot/READY. A rendering that drifted would not fail here; it would fail on
// the next resize of a VM this provisioned.
//
// The LVM mechanics come from internal/thinpool, every path from internal/paths,
// and the identity write is delegated to an `inject` callback — the same seam
// internal/migration's InjectingIdentity phase uses — so this package renders only
// what provision itself owns and the whole of it is testable with no LVM stack, no
// jailer and no root.
//
// It runs as root over SSH (a Task verb, not a daemon path), so its `sudo`
// prefixes consult no allow-list. They are written anyway because the Python's are
// and the rendered line is what a differential test compares.
package provision

import (
	"context"
	"errors"
	"fmt"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/thinpool"
)

// commands is everything this package does to the host, and the only seam it has.
// It is a superset of internal/thinpool's seam, so the LVM helpers take the same
// value straight. Outside tests there is one implementation, *run.Runner, and
// there is never a second.
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
	RunUnchecked(ctx context.Context, template string, parameters ...any) (string, error)
	OK(ctx context.Context, template string, parameters ...any) bool
	InstallFile(ctx context.Context, content string, destination string, mode string) error
	InstallDirectory(ctx context.Context, destination string, mode string) error
}

var _ commands = (*run.Runner)(nil)

// Provision lays down one VM and starts its unit.
//
// inject writes this VM's identity through the device it is handed — the seam
// internal/migration's InjectIdentity uses, and for the same reason: the mount and
// the guest files live in internal/vm, and reaching into them from here would drag
// a rootfs mount into a package whose tests must need no host. The caller chooses
// whether that write regenerates host keys (birth) or preserves them (a migration
// cutover, whose disk moved wholesale and must keep its SSH identity).
//
// Idempotent from end to end: every volume step re-activates rather than
// re-creates, every file is rewritten with the same bytes, and `systemctl enable`
// on an enabled unit is a no-op.
func Provision(
	ctx context.Context, cmd commands, params Params,
	inject func(ctx context.Context, device string) error,
) (Result, error) {
	// The UUID becomes an LV reference and a path segment in nearly every command
	// below, so it is checked before it is rendered anywhere — the guard
	// internal/hostkeys and the adoption scan make for the same reason (paths.IsUUID).
	if !paths.IsUUID(params.VirtualMachineName) {
		return Result{}, fmt.Errorf("provision-vm: %q is not a VM UUID", params.VirtualMachineName)
	}
	if len(params.CgroupArguments) == 0 {
		return Result{}, errors.New("provision-vm: no cgroup values; an empty set would un-bound the VM")
	}
	provisioning := &provisioning{
		commands:       cmd,
		params:         params,
		inject:         inject,
		virtualMachine: paths.ForVirtualMachine(params.VirtualMachineName),
		imageDirectory: paths.ImageDirectory(params.ImageName),
		rootVolume:     "atlas-vm-" + params.VirtualMachineName,
		dataVolume:     "atlas-data-" + params.VirtualMachineName,
		warm:           params.WarmSnapshotDirectory != "",
	}
	return provisioning.provision(ctx)
}

// provisioning is one run of the verb: the inputs, the paths derived from them,
// and the three facts the early steps establish that the later ones branch on.
type provisioning struct {
	commands       commands
	params         Params
	inject         func(ctx context.Context, device string) error
	virtualMachine paths.VirtualMachine
	imageDirectory string
	rootVolume     string
	dataVolume     string
	// warm is a clone of a warm golden: the disk stays a byte-exact CoW and the
	// identity travels as MMDS metadata instead of being written into it.
	warm bool
	// bootOnClone is a boot-then-hydrate migration whose dm-clone is BOTH requested
	// AND present on the host.
	bootOnClone bool
	// stageWarm is set by the volume step: the warm pair is staged only when this
	// run created the disk, or when the previous staging was never consumed. RAM
	// must never be restored over a disk that has diverged from it.
	stageWarm bool
}

// provision is main()'s sequence, and the order is the contract. The disk exists
// before its identity is written into it, the jail's files exist before the
// recursive chown hands the tree to the per-VM uid, the warm pair is hard-linked
// AFTER that chown (a chown of a hard link chowns the shared inode), and the unit
// is started last.
func (provisioning *provisioning) provision(ctx context.Context) (Result, error) {
	if err := provisioning.preflight(ctx); err != nil {
		return Result{}, err
	}
	markerWasPending, err := provisioning.freshJailTree(ctx)
	if err != nil {
		return Result{}, err
	}
	if err := provisioning.layDownVolumes(ctx, markerWasPending); err != nil {
		return Result{}, err
	}
	if err := provisioning.writeIdentity(ctx); err != nil {
		return Result{}, err
	}
	if err := provisioning.buildJail(ctx); err != nil {
		return Result{}, err
	}
	if err := provisioning.stageWarmPair(ctx); err != nil {
		return Result{}, err
	}
	if err := provisioning.writeSidecars(ctx); err != nil {
		return Result{}, err
	}
	if err := provisioning.startUnit(ctx); err != nil {
		return Result{}, err
	}
	return Result{WarmPairStaged: provisioning.stageWarm}, nil
}

// writeIdentity is step 2: this VM's identity (SSH key, network env, hostname,
// host keys, machine-id, data-disk fstab) written into the disk by mounting the LV
// directly, outside the jail, before the jailer starts. The v4 egress link goes in
// here too, so clone and rebuild get it for free.
//
// SKIPPED for a warm clone: mounting the disk would mutate it under the frozen
// RAM. The identity travels as MMDS metadata instead (step 4d); the in-guest
// freshen unit baked into the golden adopts it after resume, and on the cold-boot
// fallback the launcher preloads MMDS from the same file.
//
// SKIPPED for boot-on-clone (spec/24 §0): identity was already injected THROUGH
// the clone device in the InjectingIdentity phase. Mounting the plain LV now would
// fault — it is held busy under the clone — and would race the live read-through.
func (provisioning *provisioning) writeIdentity(ctx context.Context) error {
	if provisioning.warm || provisioning.bootOnClone {
		return nil
	}
	return provisioning.inject(ctx, thinpool.DevicePath(provisioning.rootVolume))
}

// writeSidecars is steps 6 and 7: the network.env vm-network-up reads, and the
// per-VM launcher the systemd unit execs. Both are regenerated on every
// (re)provision so they stay in sync with the row.
func (provisioning *provisioning) writeSidecars(ctx context.Context) error {
	if err := provisioning.commands.InstallFile(
		ctx, networkEnvironment(provisioning.params), provisioning.virtualMachine.NetworkEnvironment(), "0644",
	); err != nil {
		return err
	}
	return provisioning.commands.InstallFile(
		ctx, jailerLaunch(provisioning.params, provisioning.virtualMachine),
		provisioning.virtualMachine.JailerLaunch(), "0755",
	)
}

// startUnit is step 8. `enable` is instant (it writes one wants symlink) and stays
// synchronous. `start --no-block` queues the job and returns instead of blocking
// on the unit reaching active — which would mean waiting out network-online.target
// plus both ExecStartPre hooks (the disk bring-up and the dozens of ip/nft calls
// in the network bring-up). The controller marks the VM Running without waiting
// for boot anyway, so nothing downstream needs the unit active by the time this
// returns; a failing ExecStartPre still surfaces through the unit's own state
// (Restart=always), so it is not lost.
func (provisioning *provisioning) startUnit(ctx context.Context) error {
	unit := provisioning.virtualMachine.SystemdUnit()
	if _, err := provisioning.commands.Run(ctx, "sudo systemctl enable {}", unit); err != nil {
		return err
	}
	_, err := provisioning.commands.Run(ctx, "sudo systemctl start --no-block {}", unit)
	return err
}

// owner is the per-VM uid:gid pair the jail tree and its device nodes are handed
// to. The gid equals the uid by construction (atlas.networking.derive_uid).
func (provisioning *provisioning) owner() string {
	return fmt.Sprintf("%d:%d", provisioning.params.FirecrackerUID, provisioning.params.FirecrackerUID)
}
