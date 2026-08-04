// Package thinpool renders the LVM thin-pool pokes the storage-lifecycle verbs
// share — snapshot, snapshot-stop, delete-snapshot, warm-snapshot (internal/
// snapshot), the S3 backup/restore (internal/backup) and image sync/promote
// (internal/image) all move the same LVs the same way.
//
// It is a byte-for-byte port of the LogicalVolume/ThinPool operations in
// scripts/lib/atlas/lvm.py, rendered through the `commands` seam rather than a
// concrete runner. internal/vmdisk already ports the activate/import slice, and
// internal/migration re-ports the snapshot/create/remove slice through the same
// seam — this is the third caller of those templates, so they live here once
// rather than a fourth time in each verb package. The reason it is a re-port and
// not a call into vmdisk is the one internal/migration's lvm.go states: a verb
// that must stay host-free in tests renders every host poke through the seam, and
// vmdisk's helpers take a concrete *run.Runner.
//
// Everything here is a pure function over the seam: no LVM stack, no root, no
// host. The verbs' recorder tests assert the exact lvcreate/lvchange lines these
// emit, which is what a differential test against the Python compares.
package thinpool

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/frappe/boat/internal/run"
)

const (
	// VolumeGroup + PoolName mirror internal/vmdisk and internal/migration: every
	// per-VM disk is a thin LV carved from atlas/pool0. Pool is the qualified
	// reference lvcreate --thinpool takes.
	VolumeGroup = "atlas"
	PoolName    = "pool0"
	Pool        = VolumeGroup + "/" + PoolName

	// FullThreshold is lvm.py PoolUsage.FULL_THRESHOLD: a thin snapshot is free up
	// front, but every later CoW write allocates from the pool, so snapshotting an
	// almost-full pool courts a stall. Data and metadata fill are read separately
	// because they exhaust independently.
	FullThreshold = 90.0

	// baseImagePrefix names an LV that VM/snapshot lifecycle must never destroy:
	// the read-only origin many per-VM disks CoW-snapshot from. Remove refuses it.
	baseImagePrefix = "atlas-image-"
)

// commands is the host-touching seam, a strict subset of what the verb packages'
// own `commands` interfaces carry — so a verb can hand its richer seam straight
// to these functions. Outside tests the one implementation is *run.Runner.
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
	RunUnchecked(ctx context.Context, template string, parameters ...any) (string, error)
	OK(ctx context.Context, template string, parameters ...any) bool
}

var _ commands = (*run.Runner)(nil)

// Reference is atlas/<name>, the form the LVM CLI tools take. DevicePath is
// /dev/atlas/<name>, the block node. Callers never hand-build either.
func Reference(name string) string  { return VolumeGroup + "/" + name }
func DevicePath(name string) string { return "/dev/" + VolumeGroup + "/" + name }

// NameFromDevice recovers an LV name from an /dev/atlas/<name> device path: the
// basename is the name. Ports lvm.py ThinPool.from_device — Atlas passes a
// snapshot's stored device path and this reads its LV name back out.
func NameFromDevice(devicePath string) string {
	if index := strings.LastIndex(devicePath, "/"); index >= 0 {
		return devicePath[index+1:]
	}
	return devicePath
}

// Exists reports whether an LV is present. A guard, so OK is free: a probe that
// could not be made reads as absent and the mutation it guards would fail loudly
// on its own. Ports lvm.py LogicalVolume.exists.
func Exists(ctx context.Context, cmd commands, name string) bool {
	return cmd.OK(ctx, "sudo lvs --noheadings {}", Reference(name))
}

// Activate brings an LV up with -K (so an activation-skip-flagged snapshot comes
// up), settles udev, and falls back to vgmknodes — then fails loud if the node is
// still not a block device. Idempotent. Ports lvm.py LogicalVolume.activate +
// _wait_for_node; the same sequence internal/vmdisk and internal/migration use.
func Activate(ctx context.Context, cmd commands, name string) error {
	reference, devicePath := Reference(name), DevicePath(name)
	if _, err := cmd.Run(ctx, "sudo lvchange -ay -K {}", reference); err != nil {
		return err
	}
	if _, err := cmd.Run(ctx, "sudo udevadm settle"); err != nil {
		return err
	}
	if cmd.OK(ctx, "test -b {}", devicePath) {
		return nil
	}
	if _, err := cmd.Run(ctx, "sudo vgmknodes {}", VolumeGroup); err != nil {
		return err
	}
	if _, err := cmd.Run(ctx, "sudo udevadm settle"); err != nil {
		return err
	}
	if !cmd.OK(ctx, "test -b {}", devicePath) {
		return fmt.Errorf("LV %s activated but %s is not a block device", reference, devicePath)
	}
	return nil
}

// SnapshotInto creates snapshotName as a thin CoW snapshot of originName and
// activates it — instant, O(1). Idempotent: re-activates if the snapshot already
// exists, so a re-entry reuses the same image. Ports lvm.py
// LogicalVolume.snapshot_into.
func SnapshotInto(ctx context.Context, cmd commands, originName, snapshotName string) error {
	if Exists(ctx, cmd, snapshotName) {
		return Activate(ctx, cmd, snapshotName)
	}
	// No -L/--thinpool: snapshotting a thin LV inherits its pool and size.
	if _, err := cmd.Run(ctx, "sudo lvcreate -s {} -n {}", Reference(originName), snapshotName); err != nil {
		return err
	}
	return Activate(ctx, cmd, snapshotName)
}

// CreateThin creates a blank thin volume of gigabytes (its bytes private to it,
// no origin) and activates it — how a restore's fresh destination LV is born.
// Idempotent: activate-and-return if it already exists. Ports lvm.py
// ThinPool.create_thin.
func CreateThin(ctx context.Context, cmd commands, name string, gigabytes int) error {
	if Exists(ctx, cmd, name) {
		return Activate(ctx, cmd, name)
	}
	if _, err := cmd.Run(
		ctx, "sudo lvcreate --type thin --thinpool {} -V {} -n {} {}",
		Pool, strconv.Itoa(gigabytes)+"G", name, VolumeGroup,
	); err != nil {
		return err
	}
	return Activate(ctx, cmd, name)
}

// Remove removes an LV, guarded by its presence so a re-entry after a partial
// teardown is a clean no-op. Refuses the pool and base-image LVs so a malformed
// row can never destroy shared state. Ports lvm.py LogicalVolume.remove.
func Remove(ctx context.Context, cmd commands, name string) error {
	if IsProtected(name) {
		return fmt.Errorf("refusing to remove protected LV %q", name)
	}
	if !Exists(ctx, cmd, name) {
		return nil
	}
	_, err := cmd.Run(ctx, "sudo lvremove -f {}", Reference(name))
	return err
}

// IsProtected reports whether an LV is one lifecycle must never destroy: the pool
// itself or a base image. Ports lvm.py LogicalVolume.is_protected.
func IsProtected(name string) bool {
	return name == PoolName || strings.HasPrefix(name, baseImagePrefix)
}

// SizeBytes is an LV's byte count. DeviceSizeBytes reads any block device's size
// via blockdev --getsize64 — a size, not a stdout line to grep, so a caller can
// never feed a half-parsed number downstream. Run, because the OUTPUT is the
// answer. Ports lvm.py LogicalVolume.size_bytes.
func SizeBytes(ctx context.Context, cmd commands, name string) (int64, error) {
	return DeviceSizeBytes(ctx, cmd, DevicePath(name))
}

func DeviceSizeBytes(ctx context.Context, cmd commands, devicePath string) (int64, error) {
	output, err := cmd.Run(ctx, "sudo blockdev --getsize64 {}", devicePath)
	if err != nil {
		return 0, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(output), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("blockdev gave no size for %s: %q", devicePath, output)
	}
	return size, nil
}

// TooFull reports whether the thin pool's data OR metadata fill has reached
// FullThreshold — the too-full-to-snapshot / too-full-to-restore gate. Both fills
// are read because they exhaust independently. Ports lvm.py PoolUsage.
func TooFull(ctx context.Context, cmd commands) (bool, error) {
	data, err := cmd.Run(ctx, "sudo lvs --noheadings -o data_percent {}", Pool)
	if err != nil {
		return false, err
	}
	metadata, err := cmd.Run(ctx, "sudo lvs --noheadings -o metadata_percent {}", Pool)
	if err != nil {
		return false, err
	}
	return parsePercent(data) >= FullThreshold || parsePercent(metadata) >= FullThreshold, nil
}

// parsePercent reads an `lvs -o data_percent` cell: a decimal ("87.34"), or blank
// when the pool has not been written to yet. Blank → 0.0, the shell `${pct:-0}`
// default. A malformed non-blank value reads as 0.0 rather than aborting — an
// unreadable fill must not by itself fail a verb the way a real over-full pool
// should, and the mutation it gates fails loudly on its own if the pool truly
// cannot take the write. Ports lvm.py PoolUsage._parse_percent.
func parsePercent(cell string) float64 {
	cell = strings.TrimSpace(cell)
	if cell == "" {
		return 0.0
	}
	value, err := strconv.ParseFloat(cell, 64)
	if err != nil {
		return 0.0
	}
	return value
}
