package thinpool

import (
	"context"
	"fmt"
	"strconv"
)

// BaseImageLV is the read-only origin LV of an image: atlas-image-<name>. Every
// per-VM disk of that image is a thin CoW snapshot of it.
func BaseImageLV(imageName string) string { return baseImagePrefix + imageName }

// ImportBaseImageFromFile lays a pristine ext4 image FILE down as the read-only
// base LV — a thin volume of gigabytes, dd the file into it, mark it read-only.
// This is how sync-image's ext4 becomes the base every per-VM disk snapshots
// from. Idempotent: a no-op if the LV already exists (a re-synced image keeps its
// base). Returns the base LV name.
//
// Created with -V (a thin volume, not a snapshot), so the base has no origin and
// can never be orphaned. On a mid-build dd failure the half-written writable LV is
// removed. Ports lvm.py ThinPool.import_base_image; the same sequence
// internal/vmdisk.ImportBaseImage renders through a concrete runner.
func ImportBaseImageFromFile(
	ctx context.Context, cmd commands, imageName, sourceFile string, gigabytes int,
) (string, error) {
	name := BaseImageLV(imageName)
	if Exists(ctx, cmd, name) {
		return name, nil
	}
	if err := createAndFill(ctx, cmd, name, sourceFile, gigabytes); err != nil {
		return "", err
	}
	return name, nil
}

// ImportBaseImageFromLV promotes a snapshot LV into a read-only base image LV —
// the same shape as ImportBaseImageFromFile, but the source is a LOCAL LV device,
// not a downloaded file. This is how a baked snapshot becomes a first-class base
// image new VMs select with the ordinary `image` field, on the same server with
// the bytes never leaving it. Idempotent: a no-op if the target already exists.
//
// Pre-flight: the source LV must exist and is activated, so dd reads a live block
// device rather than a missing or skip-flagged node. Ports lvm.py
// ThinPool.import_base_image_from_lv.
func ImportBaseImageFromLV(
	ctx context.Context, cmd commands, sourceName, imageName string, gigabytes int,
) (string, error) {
	name := BaseImageLV(imageName)
	if Exists(ctx, cmd, name) {
		return name, nil
	}
	if !Exists(ctx, cmd, sourceName) {
		return "", fmt.Errorf("source LV %s not found; cannot promote to %s", sourceName, name)
	}
	if err := Activate(ctx, cmd, sourceName); err != nil {
		return "", err
	}
	if err := createAndFill(ctx, cmd, name, DevicePath(sourceName), gigabytes); err != nil {
		return "", err
	}
	return name, nil
}

// createAndFill is the shared tail of both imports: create the thin base LV,
// activate it, dd the source (a file or an LV device) into it, and flip it
// read-only. On a dd failure the half-populated writable LV is removed so no
// half-written base is left behind. The read-only permission is the base's
// safety: it is never mounted writable, so a stray write can't corrupt the shared
// origin; per-VM snapshots are independently writable regardless.
func createAndFill(ctx context.Context, cmd commands, name, source string, gigabytes int) error {
	if _, err := cmd.Run(
		ctx, "sudo lvcreate --type thin --thinpool {} -V {} -n {} {}",
		Pool, strconv.Itoa(gigabytes)+"G", name, VolumeGroup,
	); err != nil {
		return err
	}
	if err := Activate(ctx, cmd, name); err != nil {
		return err
	}
	if _, err := cmd.Run(
		ctx, "sudo dd if={} of={} bs=4M conv=fsync status=none", source, DevicePath(name),
	); err != nil {
		// Leave no half-written base behind.
		cmd.RunUnchecked(ctx, "sudo lvremove -f {}", Reference(name))
		return err
	}
	_, err := cmd.Run(ctx, "sudo lvchange --permission r {}", Reference(name))
	return err
}
