package migration

import "context"

// ExportSourceParams is the one thing the source cannot derive from the UUID: the
// address qemu-nbd binds. It is the source's own public IPv4 — the target dials it
// directly over plain TCP (stage 1, no tunnel).
type ExportSourceParams struct {
	BindAddress string
}

// ExportSourceResult is what cutover and cleanup need back: the port the export
// listens on, the qemu-nbd pid (cleanup kills it), and the disk sizes (the target
// sizes its dm-clone to match, and a mismatch is the "Invalid argument" bug).
type ExportSourceResult struct {
	NBDPort       int
	NBDPID        int
	RootSizeBytes int64
	DataSizeBytes int64
}

// ExportSource is the ExportingSnapshot phase: thin-snapshot the Stopped VM's
// disk(s) and serve them read-only over NBD for the target to clone from. The port
// is DERIVED from the UUID (NBDPort for the root, +1 for the data disk), not passed
// — the source and target derive the same port with no shared state.
//
// Idempotent: reuses an existing -migrate snapshot and an already-serving qemu-nbd
// (keyed by the listening port + pidfile). Ports scripts/migration-export-source.py.
func ExportSource(ctx context.Context, cmd commands, uuid string, params ExportSourceParams) (ExportSourceResult, error) {
	port, err := NBDPort(uuid)
	if err != nil {
		return ExportSourceResult{}, err
	}

	// Pool-fullness guard, same as a plain snapshot: a thin snapshot is free up front
	// but every later CoW write allocates; do not snapshot an almost-full pool.
	tooFull, err := poolPastThreshold(ctx, cmd, poolFullThreshold)
	if err != nil {
		return ExportSourceResult{}, err
	}
	if tooFull {
		return ExportSourceResult{}, errThinPoolTooFull
	}

	if _, err := cmd.Run(ctx, "sudo mkdir -p {}", runDirectory); err != nil {
		return ExportSourceResult{}, err
	}

	// 1. Root snapshot. The origin must be on this host — a missing disk means the
	//    wrong UUID or the wrong host, and is worth failing loudly for.
	if !lvExists(ctx, cmd, vmDiskLV(uuid)) {
		return ExportSourceResult{}, errSourceDiskMissing(uuid)
	}
	if err := snapshotInto(ctx, cmd, vmDiskLV(uuid), rootSnapLV(uuid)); err != nil {
		return ExportSourceResult{}, err
	}

	// 2. Data snapshot, when the VM has a data disk. Same idempotent pattern.
	hasData := lvExists(ctx, cmd, dataDiskLV(uuid))
	if hasData {
		if err := snapshotInto(ctx, cmd, dataDiskLV(uuid), dataSnapLV(uuid)); err != nil {
			return ExportSourceResult{}, err
		}
	}

	// 3. NBD exports, read-only, bound to the source's public IPv4. One qemu-nbd per
	//    disk on adjacent ports: root = port, data = port+1.
	rootPID, err := ensureNBDExport(ctx, cmd, lvDevicePath(rootSnapLV(uuid)), params.BindAddress, port)
	if err != nil {
		return ExportSourceResult{}, err
	}
	if hasData {
		if _, err := ensureNBDExport(ctx, cmd, lvDevicePath(dataSnapLV(uuid)), params.BindAddress, port+1); err != nil {
			return ExportSourceResult{}, err
		}
	}

	rootSize, err := lvSizeBytes(ctx, cmd, rootSnapLV(uuid))
	if err != nil {
		return ExportSourceResult{}, err
	}
	result := ExportSourceResult{NBDPort: port, NBDPID: rootPID, RootSizeBytes: rootSize}
	if hasData {
		dataSize, err := lvSizeBytes(ctx, cmd, dataSnapLV(uuid))
		if err != nil {
			return ExportSourceResult{}, err
		}
		result.DataSizeBytes = dataSize
	}
	return result, nil
}
