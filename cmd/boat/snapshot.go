package main

import (
	"context"
	"io"

	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/snapshot"
)

// snapshotVM takes a Stopped VM's disk snapshot — `boat snapshot-vm`, the port
// of snapshot-vm.py.
func snapshotVM(arguments []string, output io.Writer, errorOutput io.Writer) int {
	flags := newTaskFlags("snapshot-vm", errorOutput)
	uuid := flags.requiredText("virtual-machine-name")
	device := flags.requiredText("snapshot-rootfs-path")
	dataDevice := flags.text("data-snapshot-rootfs-path", "")
	if err := flags.parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	result, err := snapshot.SnapshotVM(context.Background(), run.NewRunner(errorOutput), snapshot.SnapshotVMParams{
		UUID:                   *uuid,
		SnapshotRootfsPath:     *device,
		DataSnapshotRootfsPath: *dataDevice,
	})
	if err != nil {
		return reportError(errorOutput, err)
	}
	return emit(output, errorOutput, map[string]any{
		"size_bytes":      result.SizeBytes,
		"data_size_bytes": result.DataSizeBytes,
	})
}

// snapshotStopVM captures the guest's memory on the way down — `boat
// snapshot-stop-vm`, the port of snapshot-stop-vm.py.
func snapshotStopVM(arguments []string, output io.Writer, errorOutput io.Writer) int {
	flags := newTaskFlags("snapshot-stop-vm", errorOutput)
	uuid := flags.requiredText("virtual-machine-name")
	firecrackerUID := flags.requiredNumber("atlas-fc-uid")
	if err := flags.parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	result, err := snapshot.SnapshotStopVM(context.Background(), run.NewRunner(errorOutput), snapshot.SnapshotStopParams{
		UUID:           *uuid,
		FirecrackerUID: *firecrackerUID,
	})
	if err != nil {
		return reportError(errorOutput, err)
	}
	return emit(output, errorOutput, map[string]any{
		"memory_snapshot":       result.MemorySnapshot,
		"reason":                result.Reason,
		"memory_snapshot_bytes": result.MemorySnapshotBytes,
	})
}

// warmSnapshotVM captures a running guest's disk AND memory into a durable
// pair — `boat warm-snapshot-vm`, the port of warm-snapshot-vm.py.
func warmSnapshotVM(arguments []string, output io.Writer, errorOutput io.Writer) int {
	flags := newTaskFlags("warm-snapshot-vm", errorOutput)
	uuid := flags.requiredText("virtual-machine-name")
	firecrackerUID := flags.requiredNumber("atlas-fc-uid")
	device := flags.requiredText("snapshot-rootfs-path")
	memoryDirectory := flags.requiredText("memory-directory")
	if err := flags.parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	result, err := snapshot.WarmSnapshotVM(context.Background(), run.NewRunner(errorOutput), snapshot.WarmSnapshotParams{
		UUID:               *uuid,
		FirecrackerUID:     *firecrackerUID,
		SnapshotRootfsPath: *device,
		MemoryDirectory:    *memoryDirectory,
	})
	if err != nil {
		return reportError(errorOutput, err)
	}
	return emit(output, errorOutput, map[string]any{
		"size_bytes":     result.SizeBytes,
		"memory_bytes":   result.MemoryBytes,
		"host_signature": result.HostSignature,
	})
}

// deleteSnapshotVM removes a snapshot's volumes and its memory directory —
// `boat delete-snapshot-vm`, the port of delete-snapshot-vm.py. It reports no
// result, exactly as the Python does: the removal either happened or the verb
// failed.
func deleteSnapshotVM(arguments []string, errorOutput io.Writer) int {
	flags := newTaskFlags("delete-snapshot-vm", errorOutput)
	device := flags.requiredText("snapshot-rootfs-path")
	dataDevice := flags.text("data-snapshot-rootfs-path", "")
	memoryDirectory := flags.text("memory-directory", "")
	if err := flags.parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	err := snapshot.DeleteSnapshotVM(context.Background(), run.NewRunner(errorOutput), snapshot.DeleteSnapshotVMParams{
		SnapshotRootfsPath:     *device,
		DataSnapshotRootfsPath: *dataDevice,
		MemoryDirectory:        *memoryDirectory,
	})
	if err != nil {
		return reportError(errorOutput, err)
	}
	return exitSuccess
}

// vmRestore resumes a guest from its memory snapshot, or leaves a cold boot
// alone — `boat vm-restore <uuid>`, the port of vm-restore.py.
//
// Positional, and a hook rather than a Task: firecracker-vm@.service runs it as
// an ExecStartPre, so its caller is systemd rather than the controller, and it
// takes the one argument the unit's %i already carries.
func vmRestore(arguments []string, errorOutput io.Writer) int {
	if len(arguments) != 1 {
		return usage(errorOutput)
	}
	if _, err := snapshot.RestoreVM(context.Background(), run.NewRunner(errorOutput), arguments[0]); err != nil {
		return reportError(errorOutput, err)
	}
	return exitSuccess
}

// emit writes the result line and turns a failure to write it into a failed
// verb. A verb whose work succeeded but whose result never reached the
// controller is not a success: Atlas would parse no marker and raise.
func emit(output io.Writer, errorOutput io.Writer, fields map[string]any) int {
	if err := emitResult(output, fields); err != nil {
		return reportError(errorOutput, err)
	}
	return exitSuccess
}
