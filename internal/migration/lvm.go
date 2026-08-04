package migration

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// The LVM host pokes a migration makes, each a byte-for-byte port of the
// corresponding scripts/lib/atlas/lvm.py method and matched to the shapes
// internal/vmdisk already established (thinPool reference, activate-with-fallback),
// so the two packages address the pool identically. Kept here rather than reaching
// into vmdisk because a migration phase must render these through the `commands`
// seam to stay host-free in tests, and vmdisk takes a concrete *run.Runner.

// lvExists reports whether an LV is present. A guard, so OK is free: a probe that
// could not be made reads as absent and the mutation it guards (create/remove)
// would fail loudly on its own. Matches vmdisk's `lvs --noheadings` existence probe
// and lvm.py's LogicalVolume.exists.
func lvExists(ctx context.Context, cmd commands, name string) bool {
	return cmd.OK(ctx, "sudo lvs --noheadings {}", lvReference(name))
}

// lvSizeBytes is an LV's (or connected nbd device's) byte count — a size, not a
// stdout line to grep, so a caller can never feed dm-clone a sector count off a
// half-parsed number. Run, because the OUTPUT is the answer.
func lvSizeBytes(ctx context.Context, cmd commands, name string) (int64, error) {
	return deviceSizeBytes(ctx, cmd, lvDevicePath(name))
}

// deviceSizeBytes reads any block device's byte size via blockdev --getsize64.
func deviceSizeBytes(ctx context.Context, cmd commands, devicePath string) (int64, error) {
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

// activate brings an LV up with -K (so an activation-skip-flagged snapshot comes
// up), settles udev, and falls back to vgmknodes — then fails loud if the node is
// still not a block device. Idempotent: re-activating an active LV is a no-op. The
// same sequence internal/vmdisk uses, so a migration and a boot converge a disk the
// same way. Ports lvm.py LogicalVolume.activate + _wait_for_node.
func activate(ctx context.Context, cmd commands, name string) error {
	reference := lvReference(name)
	devicePath := lvDevicePath(name)
	if _, err := cmd.Run(ctx, "sudo lvchange -ay -K {}", reference); err != nil {
		return err
	}
	if _, err := cmd.Run(ctx, "sudo udevadm settle"); err != nil {
		return err
	}
	if cmd.OK(ctx, "test -b {}", devicePath) {
		return nil
	}
	if _, err := cmd.Run(ctx, "sudo vgmknodes {}", volumeGroup); err != nil {
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

// createThin creates a blank thin volume of gigabytes (its bytes private to it, no
// origin) and activates it — how a hydration destination LV is born. Idempotent:
// activate-and-return if it already exists, so a re-entered prepare reuses the same
// disk. Ports lvm.py ThinPool.create_thin.
func createThin(ctx context.Context, cmd commands, name string, gigabytes int) error {
	if lvExists(ctx, cmd, name) {
		return activate(ctx, cmd, name)
	}
	if _, err := cmd.Run(
		ctx, "sudo lvcreate --type thin --thinpool {} -V {} -n {} {}",
		thinPool, strconv.Itoa(gigabytes)+"G", name, volumeGroup,
	); err != nil {
		return err
	}
	return activate(ctx, cmd, name)
}

// snapshotInto creates snapshotName as a thin CoW snapshot of originName and
// activates it — instant, O(1), the crash-consistent image a migration exports.
// Idempotent: re-activates if the snapshot already exists, so a re-entry after a
// crash reuses the same image. Ports lvm.py LogicalVolume.snapshot_into.
func snapshotInto(ctx context.Context, cmd commands, originName, snapshotName string) error {
	if lvExists(ctx, cmd, snapshotName) {
		return activate(ctx, cmd, snapshotName)
	}
	// No -L/--thinpool: snapshotting a thin LV inherits its pool and size.
	if _, err := cmd.Run(ctx, "sudo lvcreate -s {} -n {}", lvReference(originName), snapshotName); err != nil {
		return err
	}
	return activate(ctx, cmd, snapshotName)
}

// removeLV removes an LV, guarded by its presence so a re-entry after a partial
// teardown is a clean no-op. Ports lvm.py LogicalVolume.remove; the protected-name
// refusal is Atlas policy and stays there — a migration only ever removes its own
// -migrate snapshots, clone metadata, and the source copy it is retiring.
func removeLV(ctx context.Context, cmd commands, name string) error {
	if !lvExists(ctx, cmd, name) {
		return nil
	}
	_, err := cmd.Run(ctx, "sudo lvremove -f {}", lvReference(name))
	return err
}

// lvIsReadOnly reports whether an LV carries the LVM read-only permission flag ('r'
// in lv_attr[1]) — how a base-image ship tells an already-finalized base (read-only)
// from one still hydrating (writable). Ports receive-base's _is_read_only.
func lvIsReadOnly(ctx context.Context, cmd commands, name string) (bool, error) {
	output, err := cmd.Run(ctx, "sudo lvs --noheadings -o lv_attr {}", lvReference(name))
	if err != nil {
		return false, err
	}
	attr := strings.TrimSpace(output)
	return len(attr) > 1 && attr[1] == 'r', nil
}

// poolPastThreshold reports whether the thin pool's data OR metadata fill has
// reached percent — the source's too-full-to-snapshot gate (90) and the target's
// too-full-to-hydrate gate (80). Both fills are read because they exhaust
// independently. Ports lvm.py PoolUsage.
func poolPastThreshold(ctx context.Context, cmd commands, percent float64) (bool, error) {
	reference := lvReference(poolName)
	dataOutput, err := cmd.Run(ctx, "sudo lvs --noheadings -o data_percent {}", reference)
	if err != nil {
		return false, err
	}
	metadataOutput, err := cmd.Run(ctx, "sudo lvs --noheadings -o metadata_percent {}", reference)
	if err != nil {
		return false, err
	}
	return parsePercent(dataOutput) >= percent || parsePercent(metadataOutput) >= percent, nil
}

// parsePercent reads an `lvs -o data_percent` cell: a decimal ("87.34"), or blank
// when the pool has not been written to yet. Blank → 0.0, the shell `${pct:-0}`
// default. Isolated + pure so the parse is unit-testable with no LVM stack, the
// discipline lvm.py keeps for the same field. A malformed non-blank value reads as
// 0.0 rather than aborting: an unreadable fill must not by itself fail a migration
// the way a real over-full pool should, and the mutation it gates fails loudly on
// its own if the pool truly cannot take the write.
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
