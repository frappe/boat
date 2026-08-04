package migration

import (
	"context"
	"fmt"
)

// CollapseCloneParams tells the phase whether there is a data clone to collapse too
// (0 = none). The nbd slots are DERIVED from the UUID, matching clone-target, so the
// right nbd devices are freed.
type CollapseCloneParams struct {
	DataDiskGB int
}

// CollapseClone is the CollapseClone phase: once every block is local, collapse the
// fully-hydrated dm-clone(s) TRANSPARENTLY. In boot-then-hydrate the guest is ALREADY
// LIVE on the clone, holding the rootfs fd open, so `dmsetup remove` fails "Device or
// resource busy" (host-verified) — instead the clone is suspended, its table reloaded
// from `clone` to a `linear` map straight onto the hydrated dest LV, and resumed. The
// dm device keeps the SAME major:minor, so Firecracker's open fd stays valid: no
// re-mknod, no drive re-open, no unit blip. The source nbd client is then
// disconnected and the clone metadata dropped.
//
// Idempotent: a clone already carrying a linear table (a re-entry after collapse) or
// a missing clone device is a no-op that still converges the nbd/meta teardown.
// Guards 100% hydration before collapsing — collapsing early strands un-copied blocks
// behind a torn-down NBD, reading as zeros. Ports scripts/migration-cutover-target.py.
func CollapseClone(ctx context.Context, cmd commands, uuid string, params CollapseCloneParams) error {
	baseSlot, err := NBDBaseSlot(uuid)
	if err != nil {
		return err
	}
	// root = base+0, data = base+1 — the same per-VM block clone-target attached.
	type target struct {
		key  string
		slot int
	}
	targets := []target{{uuid, baseSlot}}
	if params.DataDiskGB > 0 {
		targets = append(targets, target{uuid + "-data", baseSlot + 1})
	}
	for _, one := range targets {
		if err := collapseOne(ctx, cmd, one.key, one.slot); err != nil {
			return err
		}
	}
	return nil
}

func collapseOne(ctx context.Context, cmd commands, key string, slot int) error {
	name := vmCloneName(key)
	// No clone device (cold-path re-entry, or a prior collapse that removed the node):
	// nothing to collapse, but still converge the nbd client and meta LV teardown.
	if !cmd.OK(ctx, "sudo dmsetup info {}", name) {
		converge(ctx, cmd, key, slot)
		return nil
	}
	table, err := cmd.Run(ctx, "sudo dmsetup table {}", name)
	if err != nil {
		return err
	}
	if isLinearTable(table) {
		// Already collapsed (idempotent re-entry) — still converge the teardown.
		converge(ctx, cmd, key, slot)
		return nil
	}
	// Only collapse a fully-hydrated device. The controller calls at 100%, but a
	// lost-task re-entry could arrive early, and mapping the linear dest before every
	// region is copied would read holes as zeros.
	status, err := cmd.Run(ctx, "sudo dmsetup status {}", name)
	if err != nil {
		return err
	}
	if !fullyHydrated(status) {
		return fmt.Errorf("refusing to collapse %s: not fully hydrated (%q)", name, status)
	}
	// Transparent collapse. The dest is read from the clone's OWN table (the device it
	// hydrated into), keeping the same dm major:minor so the guest's open fd survives.
	destDevice := cloneTableDest(table)
	sectors, err := cloneSectors(table)
	if err != nil {
		return err
	}
	if _, err := cmd.Run(ctx, "sudo dmsetup suspend {}", name); err != nil {
		return err
	}
	if _, err := cmd.Run(ctx, "sudo dmsetup reload {} --table {}", name, fmt.Sprintf("0 %d linear %s 0", sectors, destDevice)); err != nil {
		return err
	}
	if _, err := cmd.Run(ctx, "sudo dmsetup resume {}", name); err != nil {
		return err
	}
	converge(ctx, cmd, key, slot)
	return nil
}

// converge is the post-collapse teardown, also run on every idempotent re-entry so a
// half-finished collapse fully settles: disconnect the source nbd client and drop the
// now-unused clone-metadata LV.
func converge(ctx context.Context, cmd commands, key string, slot int) {
	// Unconditional -d, not gated on `nbd-client -check`: -check LIES (it reports
	// connected off a stale kernel binding whose process has died), and -d is
	// idempotent — harmless on an already-free slot, the same unconditional
	// disconnect receive-base's finalize makes. By now the linear reload no longer
	// reads through NBD, so the device is not pinned and -d takes.
	cmd.RunUnchecked(ctx, "sudo nbd-client -d /dev/nbd{}", slot)
	meta := cloneMetaLV(key)
	if lvExists(ctx, cmd, meta) {
		cmd.RunUnchecked(ctx, "sudo lvremove -f {}", lvReference(meta))
	}
}
