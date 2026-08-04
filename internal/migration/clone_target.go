package migration

import "context"

// CloneTargetParams is what the target cannot derive from the UUID: the image the
// kernel comes from at cutover, the disk sizes to size the destination LVs to (>=
// the source), and the source's reachable address the nbd client dials.
type CloneTargetParams struct {
	ImageName  string
	DiskGB     int
	DataDiskGB int
	SourceHost string
}

// CloneTargetResult hands back the read-through devices the guest will boot on, so
// the caller can hand them to the fenced cutover boot without re-deriving them.
type CloneTargetResult struct {
	RootCloneDevice string
	DataCloneDevice string
}

// CloneTarget is the TargetPreparing phase's clone step: pre-flight the migration
// deps, create fresh local thin LV(s), connect nbd client(s) to the source's NBD
// export over plain TCP, and build the dm-clone device(s). The target VM's disk then
// reads through to the source over NBD while PollHydration copies every block
// locally in the background. Identity inject and the boot are NOT here — they run at
// cutover once the clone is laid down. The nbd port and client slots are DERIVED
// from the UUID, so the source and target name the same devices with no shared
// state. Idempotent: every step probes its artifact before acting. Ports
// scripts/migration-clone-target.py (PHASE=prepare).
func CloneTarget(ctx context.Context, cmd commands, uuid string, params CloneTargetParams) (CloneTargetResult, error) {
	port, err := NBDPort(uuid)
	if err != nil {
		return CloneTargetResult{}, err
	}
	baseSlot, err := NBDBaseSlot(uuid)
	if err != nil {
		return CloneTargetResult{}, err
	}

	// 0. Migration-dep pre-flight. These ship at bootstrap, but re-assert loudly here
	//    rather than fail deep in dmsetup/nbd-client.
	for _, module := range []string{"nbd", "dm_clone"} {
		if !cmd.OK(ctx, "sudo modprobe {}", module) {
			return CloneTargetResult{}, errKernelModuleMissing(module)
		}
	}
	if !cmd.OK(ctx, "which nbd-client") {
		return CloneTargetResult{}, errNBDClientMissing
	}

	// 1. Image present (the kernel comes from it at cutover) — both the base LV and
	//    the on-disk image directory.
	if !lvExists(ctx, cmd, baseImageLV(params.ImageName)) {
		return CloneTargetResult{}, errBaseImageMissing(params.ImageName)
	}
	if !cmd.OK(ctx, "test -d {}", imageDirectory(params.ImageName)) {
		return CloneTargetResult{}, errImageDirectoryMissing(params.ImageName)
	}

	// 2. Pool headroom for hydration's CoW writes.
	tooFull, err := poolPastThreshold(ctx, cmd, poolHydrationThreshold)
	if err != nil {
		return CloneTargetResult{}, err
	}
	if tooFull {
		return CloneTargetResult{}, errThinPoolTooFull
	}

	// 3. Fresh local thin LV(s) the clone hydrates INTO. Idempotent.
	if err := createThin(ctx, cmd, vmDiskLV(uuid), params.DiskGB); err != nil {
		return CloneTargetResult{}, err
	}
	hasData := params.DataDiskGB > 0
	if hasData {
		if err := createThin(ctx, cmd, dataDiskLV(uuid), params.DataDiskGB); err != nil {
			return CloneTargetResult{}, err
		}
	}

	// 4. Repair a wedged stack (dead source client) BEFORE (re)building it — the clone
	//    pins the nbd device open, so it must come down first. A healthy clone is left
	//    untouched (progress preserved).
	dropCloneIfSourceDead(ctx, cmd, vmCloneName(uuid), baseSlot)
	if hasData {
		dropCloneIfSourceDead(ctx, cmd, vmCloneName(uuid+"-data"), baseSlot+1)
	}

	// 5. nbd clients straight to the source over plain TCP (no tunnel this stage),
	//    size-verified against the freshly-created dest LVs.
	rootSize, err := lvSizeBytes(ctx, cmd, vmDiskLV(uuid))
	if err != nil {
		return CloneTargetResult{}, err
	}
	rootNBD, err := ensureNBDClient(ctx, cmd, params.SourceHost, port, baseSlot, rootSize)
	if err != nil {
		return CloneTargetResult{}, err
	}
	dataNBD := ""
	if hasData {
		dataSize, err := lvSizeBytes(ctx, cmd, dataDiskLV(uuid))
		if err != nil {
			return CloneTargetResult{}, err
		}
		dataNBD, err = ensureNBDClient(ctx, cmd, params.SourceHost, port+1, baseSlot+1, dataSize)
		if err != nil {
			return CloneTargetResult{}, err
		}
	}

	// 6. dm-clone device(s). Idempotent: skip if the mapper device already exists.
	if err := ensureDMClone(ctx, cmd, vmCloneName(uuid), cloneMetaLV(uuid), vmDiskLV(uuid), rootNBD); err != nil {
		return CloneTargetResult{}, err
	}
	result := CloneTargetResult{RootCloneDevice: "/dev/mapper/" + vmCloneName(uuid)}
	if hasData {
		if err := ensureDMClone(ctx, cmd, vmCloneName(uuid+"-data"), cloneMetaLV(uuid+"-data"), dataDiskLV(uuid), dataNBD); err != nil {
			return CloneTargetResult{}, err
		}
		result.DataCloneDevice = "/dev/mapper/" + vmCloneName(uuid+"-data")
	}
	return result, nil
}
