package vm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/run"
)

const (
	// The memory file is the size of the guest's RAM. Require that much plus a
	// margin to be free, so a snapshot can never wedge the host filesystem
	// against full — the host that runs out of space mid-snapshot loses far more
	// than the one that cold-boots next time.
	freeSpaceMarginBytes = 256 * 1024 * 1024
	bytesPerMebibyte     = 1024 * 1024

	// The jq expression that reads the guest's RAM size out of firecracker.json.
	guestMemoryQuery = `."machine-config".mem_size_mib`

	// The snapshot directory is written by the JAILED Firecracker, which runs as
	// the per-VM uid, so it is created 0700 and chowned to that uid.
	memorySnapshotDirectoryMode = "0700"

	// A full snapshot: vmstate plus the guest's RAM. The two paths are
	// jail-RELATIVE because the jailed Firecracker resolves them after its
	// chroot, exactly as firecracker.json names rootfs.ext4 and vmlinux.
	memorySnapshotBody = `{"snapshot_type": "Full", "snapshot_path": "` +
		paths.MemorySnapshotVMStateInJail + `", "mem_file_path": "` +
		paths.MemorySnapshotMemoryInJail + `"}`
)

// SleepResult says which of the two sleeps happened.
//
// Both leave the VM stopped and marked sleeping; only the next wake's speed
// differs, so a caller does not have to branch on this. It is reported because
// Reason is the only record of WHY a VM will cold-boot on its next wake, and
// that is a host fault an operator can fix — a launcher that predates memory
// snapshots wants a re-provision, a full filesystem wants space.
type SleepResult struct {
	MemorySnapshot      bool
	Reason              string
	MemorySnapshotBytes int64
}

// Sleep parks a VM: capture its memory, stop its unit, and mark it sleeping.
//
// The marker is the load-bearing artifact, not the snapshot. It is what the
// unit's ConditionPathExists=! condition reads, so a sleeping VM does not come back on
// the next host reboot, and it is what Observe reports Sleeping from. Which is
// why every path below writes it AFTER the unit stops, including the paths that
// gave up on the snapshot: a VM that ends this verb stopped but unmarked would
// be resurrected by the next reboot with nobody having asked.
//
// The snapshot is the optimisation on top. Any failure taking it falls back to
// a plain stop with a reason, because the caller asked for the VM's RAM back
// and that is what a plain stop delivers.
func (manager *Manager) Sleep(
	ctx context.Context, runner *run.Runner, uuid string, request SleepRequest,
) (SleepResult, error) {
	commands := manager.commandsFor(runner)
	files := manager.filesFor(uuid)
	if err := manager.requireWakeTrap(); err != nil {
		return SleepResult{}, err
	}
	reason, err := manager.memorySnapshotPreflight(ctx, commands, files)
	if err != nil {
		return SleepResult{}, err
	}
	if reason == "" {
		if err := manager.captureMemorySnapshot(ctx, commands, files, request.FirecrackerUID); err != nil {
			reason = fmt.Sprintf("snapshot failed: %v", err)
		}
	}
	if reason != "" {
		if err := manager.sleepWithoutMemorySnapshot(ctx, commands, files, reason); err != nil {
			return SleepResult{Reason: reason}, err
		}
		return SleepResult{Reason: reason}, manager.parkForWake(ctx, runner, uuid)
	}
	result, err := manager.sleepWithMemorySnapshot(ctx, commands, files)
	if err != nil {
		return result, err
	}
	return result, manager.parkForWake(ctx, runner, uuid)
}

// SleepRequest is how a caller asks for a sleep.
type SleepRequest struct {
	// FirecrackerUID is the VM's own uid, the one the jailer de-privileges to.
	// The snapshot directory is chowned to it because the JAILED Firecracker is
	// what writes the vmstate and memory files into it; a root-owned directory
	// there produces a snapshot that silently fails to be written.
	FirecrackerUID int
}

// requireWakeTrap refuses to sleep when nothing on this host could wake the VM
// again.
//
// The wake trap is what turns an inbound SYN into a start. Without it a slept
// VM is still parked — its /128 routes into the atlas-park0 dummy — so it
// answers nothing and stays dark until an operator clicks Start. That is
// strictly worse than leaving the VM awake, and it fails SILENTLY: the sleep
// succeeds and the tenant just sees a black hole.
//
// This is not hypothetical. A host that had its scripts synced but was never
// re-bootstrapped carried the trap's code with no unit file (units ship at
// bootstrap, not with a script sync); VMs slept there and could not be woken by
// traffic at all. Fail loudly and leave the VM running instead — the operation
// record says why.
//
// What it asserts is BOAT'S OWN trap, resident in this process (internal/park,
// started by the daemon's background loops). It used to ask systemd about
// atlas-wake-trap.service — the Python daemon — and that named the wrong reflex
// in both directions:
//
//   - A host Boat bootstrapped has no such unit at all, so every sleep on the
//     correctly configured host was refused as unsafe.
//   - A host carrying both is worse, because the Python daemon deliberately
//     STANDS DOWN while boat.service is active and stays enabled and active while
//     it does (scripts/atlas-wake-trap.py, _boat_owns_the_wake_reflex) — so
//     `is-active` answered yes for a daemon that had stopped polling, and the
//     gate passed for a reflex nobody was running. A gate that reports a
//     precondition it did not check is worse than no gate.
//
// The two decisions compose because neither asks the other a question: Python
// stands down when boat.service is up, Boat asserts the loop in its own process,
// and there is no unit whose state means one thing to one of them and something
// else to the other.
//
// A hard precondition, so it runs before anything touches the VM: a trap-less
// host never gets as far as pausing vCPUs or stopping a unit.
func (manager *Manager) requireWakeTrap() error {
	if manager.wakeTrapResident() {
		return nil
	}
	return errors.New(
		"refusing to sleep: this host's wake trap is not running, so an inbound connection could " +
			"not wake the VM; the trap is one of boat's background loops, so check the daemon's log " +
			"for a loop that ended",
	)
}

// memorySnapshotPreflight returns the reason a memory snapshot cannot be taken,
// or "" to go ahead. A returned error is different from a reason: the reason
// says "stop the plain way", the error says the host could not be read at all.
func (manager *Manager) memorySnapshotPreflight(
	ctx context.Context, commands commands, files virtualMachineFiles,
) (string, error) {
	// A launcher generated before memory snapshots existed always passes
	// --config-file, so a marker written now would strand the next start
	// (Firecracker refuses to load a snapshot over a booted guest).
	if !commands.OK(ctx, "sudo grep -q snapshot/READY {}", files.jailerLaunch) {
		return "launcher predates memory snapshots; re-provision the VM to enable fast start", nil
	}
	if !commands.OK(ctx, "sudo test -S {}", files.apiSocket) {
		return "API socket missing; is the VM running?", nil
	}
	// Drop the previous snapshot before measuring: its space is reclaimed, and a
	// stale marker must not survive a failure further down.
	if _, err := commands.Run(ctx, "sudo rm -rf {}", files.memorySnapshotDirectory); err != nil {
		return "", err
	}
	return manager.freeSpaceForMemorySnapshot(ctx, commands, files)
}

func (manager *Manager) freeSpaceForMemorySnapshot(
	ctx context.Context, commands commands, files virtualMachineFiles,
) (string, error) {
	output, err := commands.Run(ctx, "sudo jq -r {} {}", guestMemoryQuery, files.firecrackerConfig)
	if err != nil {
		return "", err
	}
	memoryMebibytes, err := strconv.ParseInt(strings.TrimSpace(output), 10, 64)
	if err != nil {
		return "", fmt.Errorf("machine-config mem_size_mib in %s: %w", files.firecrackerConfig, err)
	}
	output, err = commands.Run(ctx, "df --output=avail -B1 {}", paths.AtlasRoot)
	if err != nil {
		return "", err
	}
	available, err := availableBytes(output)
	if err != nil {
		return "", err
	}
	if available < memoryMebibytes*bytesPerMebibyte+freeSpaceMarginBytes {
		return fmt.Sprintf(
			"not enough free space for a %d MiB memory file (%d B available)", memoryMebibytes, available,
		), nil
	}
	return "", nil
}

// availableBytes reads `df --output=avail -B1`, whose first line is the AVAIL
// header and whose second is the number.
func availableBytes(output string) (int64, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("df reported no free space line: %q", output)
	}
	return strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64)
}

// captureMemorySnapshot freezes the guest and writes the vmstate/memory pair,
// then the marker.
//
// The marker asserts a COMPLETE pair, so both files are checked non-empty
// before it is written. Everything a later start does with the snapshot keys
// off that marker, so a half-written pair that carried one would be loaded and
// would fail the start.
func (manager *Manager) captureMemorySnapshot(
	ctx context.Context, commands commands, files virtualMachineFiles, firecrackerUID int,
) error {
	if err := commands.InstallDirectory(
		ctx, files.memorySnapshotDirectory, memorySnapshotDirectoryMode,
	); err != nil {
		return err
	}
	owner := fmt.Sprintf("%d:%d", firecrackerUID, firecrackerUID)
	if _, err := commands.Run(ctx, "sudo chown {} {}", owner, files.memorySnapshotDirectory); err != nil {
		return err
	}
	socketDirectory, socketName := files.apiSocketDirectory, files.apiSocketName
	if err := commands.FirecrackerAPI(
		ctx, socketDirectory, socketName, "PATCH", "/vm", pauseStateBody,
	); err != nil {
		return err
	}
	if err := commands.FirecrackerAPI(
		ctx, socketDirectory, socketName, "PUT", "/snapshot/create", memorySnapshotBody,
	); err != nil {
		return err
	}
	for _, file := range []string{files.memorySnapshotVMState, files.memorySnapshotMemory} {
		if !commands.OK(ctx, "sudo test -s {}", file) {
			return fmt.Errorf("snapshot file missing or empty: %s", file)
		}
	}
	_, err := commands.Run(ctx, "sudo touch {}", files.memorySnapshotMarker)
	return err
}

// sleepWithMemorySnapshot stops the unit on top of a complete snapshot and
// reports the memory file's size, which is the RAM this sleep actually freed.
func (manager *Manager) sleepWithMemorySnapshot(
	ctx context.Context, commands commands, files virtualMachineFiles,
) (SleepResult, error) {
	if err := manager.stopAndMarkSleeping(ctx, commands, files); err != nil {
		return SleepResult{MemorySnapshot: true}, err
	}
	output, err := commands.Run(ctx, "sudo stat -c %s {}", files.memorySnapshotMemory)
	if err != nil {
		return SleepResult{MemorySnapshot: true}, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(output), 10, 64)
	if err != nil {
		return SleepResult{MemorySnapshot: true}, fmt.Errorf("memory file size: %w", err)
	}
	return SleepResult{MemorySnapshot: true, MemorySnapshotBytes: size}, nil
}

// sleepWithoutMemorySnapshot is the fallback: no fast wake, but still a sleep.
// The stale marker goes first — a partial snapshot must never be restored — and
// the sleeping marker still lands, so the reboot suppression takes effect
// either way.
func (manager *Manager) sleepWithoutMemorySnapshot(
	ctx context.Context, commands commands, files virtualMachineFiles, reason string,
) error {
	if _, err := commands.Run(ctx, "sudo rm -f {}", files.memorySnapshotMarker); err != nil {
		return err
	}
	if err := manager.stopAndMarkSleeping(ctx, commands, files); err != nil {
		return fmt.Errorf("%w (falling back to a plain stop: %s)", err, reason)
	}
	return nil
}

// stopAndMarkSleeping is the order both paths share, and it is an order rather
// than two steps: the marker is written after the stop, because a marker
// written first would make the stop's own restart-on-failure a no-op and leave
// a VM marked sleeping while it is still running.
func (manager *Manager) stopAndMarkSleeping(
	ctx context.Context, commands commands, files virtualMachineFiles,
) error {
	if _, err := commands.Run(ctx, "sudo systemctl stop {}", files.unit); err != nil {
		return err
	}
	_, err := commands.Run(ctx, "sudo touch {}", files.sleepingMarker)
	return err
}

// parkForWake arms the trap that is the only way a slept VM ever comes back on
// its own.
//
// The stop above tore down every scrap of the VM's networking — that is what
// frees the RAM — so until this runs its address is unrouted and no packet can
// reach the host to trigger a wake. A failure here is therefore fatal, not
// best-effort: a VM that is asleep and untrapped is a VM that answers nothing
// and that no traffic can revive, and reporting that as a successful sleep
// would hide an outage behind a green Task.
func (manager *Manager) parkForWake(ctx context.Context, runner *run.Runner, uuid string) error {
	if err := manager.park(ctx, runner, uuid); err != nil {
		return fmt.Errorf("the virtual machine is stopped but could not be parked for wake: %w", err)
	}
	return nil
}

// Wake starts a sleeping VM: remove the marker, then start the unit.
//
// The marker comes off FIRST. The unit's ConditionPathExists=! condition sees a marker
// and silently declines to start — exit 0, unit skipped — so a start with the
// marker still in place reports success and leaves the VM down. Removing it
// first also means a wake that follows a failed wake still clears it.
//
// The start itself is Start, not a bare `systemctl start`: a sleeping VM is
// precisely the VM with a memory snapshot staged, so it is the one most likely
// to hit the failed-restore case Start exists to handle.
//
// The network unpark is deliberately NOT here. The started unit's own
// ExecStartPre calls it before it rebuilds the real path, which is the only
// place it can happen in the right order — unparking from here would remove the
// SYN trap while the VM is still down, so the client's retransmit would arrive
// at a host with nothing listening and nothing left to trap it.
func (manager *Manager) Wake(ctx context.Context, runner *run.Runner, uuid string) error {
	commands := manager.commandsFor(runner)
	files := manager.filesFor(uuid)
	if _, err := commands.Run(ctx, "sudo rm -f {}", files.sleepingMarker); err != nil {
		return err
	}
	_, err := manager.Start(ctx, runner, uuid)
	return err
}
