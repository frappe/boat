// Package snapshot captures a VM's state as a durable artifact: a disk-only LVM
// thin snapshot of a Stopped VM (SnapshotVM), a memory + disk snapshot taken as a
// VM stops so its next start resumes instead of cold-booting (SnapshotStopVM), a
// warm golden capture of a Running VM's paused RAM and disk at one instant for
// fan-out to N clones (WarmSnapshotVM), and the removal of any of these
// (DeleteSnapshotVM).
//
// It ports scripts/{snapshot-vm,snapshot-stop-vm,warm-snapshot-vm,
// delete-snapshot-vm}.py, comments and all. The LVM thin-snapshot mechanics live
// in internal/thinpool (shared with the backup and image verbs); this package is
// the snapshot lifecycle on top of them — the Firecracker pause/snapshot/resume
// dance and the durable staging that a plain LV snapshot does not do.
//
// Everything here is a pure function over the run seam, so the whole package is
// unit-testable with no Firecracker, no LVM stack and no root: the command
// sequence each verb emits is exactly what a differential test against the Python
// compares. Every verb is idempotent — a replay re-runs the work.
package snapshot

import (
	"context"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/run"
)

// commands is everything this package does to the host, and the only seam it has.
// It is a superset of internal/thinpool's seam, so a verb hands the same value
// straight to the shared LVM helpers. Outside tests there is one implementation,
// *run.Runner, and there is never a second.
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
	RunUnchecked(ctx context.Context, template string, parameters ...any) (string, error)
	OK(ctx context.Context, template string, parameters ...any) bool
	InstallFile(ctx context.Context, content string, destination string, mode string) error
	InstallDirectory(ctx context.Context, destination string, mode string) error
	FirecrackerAPI(ctx context.Context, socketDirectory, socketName, method, apiPath, body string) error
}

var _ commands = (*run.Runner)(nil)

const (
	// The Firecracker /vm state transitions the memory captures drive. Paused
	// freezes the vCPUs so the RAM stops changing; Resumed lets a warm capture's
	// golden VM run on after the pair is written. Byte-identical to the bodies
	// internal/vm's pause/sleep verbs send.
	pausedStateBody  = `{"state": "Paused"}`
	resumedStateBody = `{"state": "Resumed"}`

	// A Full snapshot: the vmstate plus the guest's RAM. The two paths are
	// jail-RELATIVE because the jailed Firecracker resolves them after its chroot,
	// exactly as firecracker.json names rootfs.ext4 and vmlinux.
	memorySnapshotBody = `{"snapshot_type": "Full", "snapshot_path": "` +
		paths.MemorySnapshotVMStateInJail + `", "mem_file_path": "` +
		paths.MemorySnapshotMemoryInJail + `"}`

	// The snapshot directory is written by the JAILED Firecracker (the per-VM uid),
	// so it is created 0700 and chowned to that uid; a root-owned directory there
	// produces a snapshot that silently fails to be written.
	memorySnapshotDirectoryMode = "0700"
)
