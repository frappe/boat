package snapshot

import (
	"context"
	"fmt"
	"strings"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/thinpool"
)

// WarmSnapshotParams selects the Running VM to capture, the uid its jailed
// Firecracker writes under, the disk snapshot's device path, and the durable
// directory the memory pair is staged into.
type WarmSnapshotParams struct {
	UUID           string
	FirecrackerUID int
	// SnapshotRootfsPath is the disk snapshot's /dev/atlas/<name> device path.
	SnapshotRootfsPath string
	// MemoryDirectory is the durable directory for vmstate.bin/mem.bin/
	// host-signature.json. It MUST live under /var/lib/atlas/snapshots.
	MemoryDirectory string
}

// WarmSnapshotResult is the captured sizes and the host signature, so the
// controller can gate a future restore on a matching host.
type WarmSnapshotResult struct {
	SizeBytes     int64  // disk snapshot LV size
	MemoryBytes   int64  // on-disk size of the captured memory file
	HostSignature string // compact JSON: cpu model/flags/microcode, kernel, FC version
}

// WarmSnapshotVM captures a WARM golden snapshot of a Running (pre-warmed) VM: the
// guest's full memory state AND an LVM thin snapshot of its disk, both at one
// paused instant, written to a durable artifact location. The fan-out producer —
// N future clones each restore this pair instead of cold-booting.
//
// The pause/resume ordering is the correctness of the whole verb. The memory pair
// and the disk snapshot are captured at the SAME paused instant, so the frozen
// RAM's filesystem cache matches the captured disk exactly (the pair is only valid
// together); and the VM is ALWAYS resumed, even when the capture fails, because a
// golden VM left paused is an outage. No READY marker is ever written in the
// source jail — the golden VM itself must never resume from this pair.
//
// Unlike SnapshotStopVM (the opt-in fast stop, which falls back to a plain stop),
// this is an operator bake step: any failure is loud. Ports
// scripts/warm-snapshot-vm.py.
func WarmSnapshotVM(ctx context.Context, cmd commands, params WarmSnapshotParams) (WarmSnapshotResult, error) {
	vm := paths.ForVirtualMachine(params.UUID)
	if !strings.HasPrefix(params.MemoryDirectory, paths.SnapshotsDirectory+"/") {
		return WarmSnapshotResult{}, fmt.Errorf(
			"memory directory must live under %s: %s", paths.SnapshotsDirectory, params.MemoryDirectory,
		)
	}
	if err := warmPreflight(ctx, cmd, vm, params.UUID); err != nil {
		return WarmSnapshotResult{}, err
	}

	snapshotName := thinpool.NameFromDevice(params.SnapshotRootfsPath)
	if err := capturePausedInstant(ctx, cmd, vm, params); err != nil {
		return WarmSnapshotResult{}, err
	}

	signature, err := readHostSignature(ctx, cmd)
	if err != nil {
		return WarmSnapshotResult{}, err
	}
	if err := stageDurable(ctx, cmd, vm, params.MemoryDirectory, signature); err != nil {
		return WarmSnapshotResult{}, err
	}

	memoryBytes, err := memoryFileSize(ctx, cmd, params.MemoryDirectory+"/mem.bin")
	if err != nil {
		return WarmSnapshotResult{}, err
	}
	size, err := thinpool.SizeBytes(ctx, cmd, snapshotName)
	if err != nil {
		return WarmSnapshotResult{}, err
	}
	signatureJSON, err := signatureResultJSON(signature)
	if err != nil {
		return WarmSnapshotResult{}, err
	}
	return WarmSnapshotResult{SizeBytes: size, MemoryBytes: memoryBytes, HostSignature: signatureJSON}, nil
}

// warmPreflight fails loud on any condition that makes a warm capture unsafe: no
// running VM, a data disk (unsupported — the frozen RAM references only the root
// disk), an over-full pool, or too little room for the RAM-sized memory file.
func warmPreflight(ctx context.Context, cmd commands, vm paths.VirtualMachine, uuid string) error {
	if !cmd.OK(ctx, "sudo test -S {}", vm.APISocket()) {
		return fmt.Errorf("API socket missing; is the VM running?")
	}
	if thinpool.Exists(ctx, cmd, "atlas-data-"+uuid) {
		return fmt.Errorf("warm snapshots do not support a data disk; bake from a VM without one")
	}
	tooFull, err := thinpool.TooFull(ctx, cmd)
	if err != nil {
		return err
	}
	if tooFull {
		return fmt.Errorf("thin pool %s too full for a safe snapshot", thinpool.Pool)
	}
	reason, err := checkMemoryFileSpace(ctx, cmd, vm)
	if err != nil {
		return err
	}
	if reason != "" {
		return fmt.Errorf("%s", reason)
	}
	return nil
}

// capturePausedInstant pauses the vCPUs, writes the memory pair and the disk
// snapshot while paused, and ALWAYS resumes — the try/finally the Python spells
// out. A capture error takes precedence over a resume error in the report, but the
// resume is attempted regardless, so a failed capture never leaves the golden VM
// frozen.
func capturePausedInstant(ctx context.Context, cmd commands, vm paths.VirtualMachine, params WarmSnapshotParams) error {
	if err := cmd.FirecrackerAPI(
		ctx, vm.APISocketDirectory(), vm.APISocketName(), "PATCH", "/vm", pausedStateBody,
	); err != nil {
		return err
	}
	captureErr := captureWhilePaused(ctx, cmd, vm, params)
	resumeErr := cmd.FirecrackerAPI(
		ctx, vm.APISocketDirectory(), vm.APISocketName(), "PATCH", "/vm", resumedStateBody,
	)
	if captureErr != nil {
		return captureErr
	}
	return resumeErr
}

// captureWhilePaused writes the memory pair into the jail, then takes the disk
// snapshot at the same paused instant — the frozen RAM references exactly these
// blocks.
func captureWhilePaused(ctx context.Context, cmd commands, vm paths.VirtualMachine, params WarmSnapshotParams) error {
	if err := installAndOwnMemoryDirectory(ctx, cmd, vm, params.FirecrackerUID); err != nil {
		return err
	}
	if err := writeAndVerifyMemoryPair(ctx, cmd, vm); err != nil {
		return err
	}
	return thinpool.SnapshotInto(ctx, cmd, "atlas-vm-"+params.UUID, thinpool.NameFromDevice(params.SnapshotRootfsPath))
}

// stageDurable moves the pair out of the jail to the durable directory (same
// filesystem — an instant rename) and records the host signature beside it. 0644
// inodes: the files are later hard-linked into clone jails and mapped read-only by
// jailed Firecrackers running under arbitrary per-VM uids (MAP_PRIVATE never
// writes back). The source jail's snapshot directory is removed — no marker was
// ever written, so the golden VM can never resume from it.
func stageDurable(
	ctx context.Context, cmd commands, vm paths.VirtualMachine, memoryDirectory string, signature hostSignature,
) error {
	if err := cmd.InstallDirectory(ctx, memoryDirectory, "0755"); err != nil {
		return err
	}
	for _, name := range []string{"vmstate.bin", "mem.bin"} {
		source := vm.MemorySnapshotDirectory() + "/" + name
		destination := memoryDirectory + "/" + name
		if _, err := cmd.Run(ctx, "sudo mv {} {}", source, destination); err != nil {
			return err
		}
		if _, err := cmd.Run(ctx, "sudo chown root:root {}", destination); err != nil {
			return err
		}
		if _, err := cmd.Run(ctx, "sudo chmod 0644 {}", destination); err != nil {
			return err
		}
	}
	content, err := signatureFileContent(signature)
	if err != nil {
		return err
	}
	if err := cmd.InstallFile(ctx, content, memoryDirectory+"/host-signature.json", "0644"); err != nil {
		return err
	}
	_, err = cmd.Run(ctx, "sudo rm -rf {}", vm.MemorySnapshotDirectory())
	return err
}
