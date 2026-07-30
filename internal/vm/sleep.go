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

// Sleep parks a VM: capture its memory, stop its unit, mark it sleeping and arm
// the trap that brings it back.
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
//
// Its first question is whether the VM is already asleep, because a replay of
// this verb must converge rather than repeat: see reassertSleep for what a
// second sleep used to cost the first one's snapshot.
func (manager *Manager) Sleep(
	ctx context.Context, runner *run.Runner, uuid string, request SleepRequest,
) (SleepResult, error) {
	commands := manager.commandsFor(runner)
	files := manager.filesFor(uuid)
	if err := manager.requireWakeTrap(); err != nil {
		return SleepResult{}, err
	}
	if commands.OK(ctx, "sudo test -f {}", files.sleepingMarker) {
		return manager.reassertSleep(ctx, runner, uuid)
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
		return SleepResult{Reason: reason}, manager.sleepWithoutMemorySnapshot(ctx, runner, uuid, reason)
	}
	return manager.sleepWithMemorySnapshot(ctx, runner, uuid)
}

// reassertSleep is what a replay of this verb does to a VM that is already
// asleep: re-assert the three things a sleep leaves behind, and report what the
// host already holds.
//
// Sleep has to be idempotent, and not because retries are tidy. §11.5 concludes
// every interrupted operation as a Failure *on the stated grounds that every
// verb is idempotent, so a retry re-runs the work* — and the operation a crash
// most plausibly interrupts here is one that stopped the unit and wrote the
// marker but had not yet armed the trap. That VM is asleep and unreachable, the
// state parkForWake calls fatal, and the designed recovery is exactly this
// replay. So a replay must converge rather than refuse: refusing would hand
// Atlas a red Task for a VM that is correctly asleep and would leave the one
// that is not asleep-and-trapped with no path back at all.
//
// What it must never do is walk the fresh-sleep path again. That path's first
// act is to `rm -rf` the snapshot directory it is about to rewrite, guarded on
// `test -S` against a socket inode that OUTLIVES the Firecracker that bound it —
// so a replay could pass the guard, destroy a perfectly good memory snapshot,
// fail to talk to the dead socket, take the fallback (which removes the READY
// marker for good measure) and report Success. The VM then cold-boots on its
// next wake with a green Task beside it, which is the failure mode this whole
// package exists to end.
//
// So the snapshot is neither retaken nor dropped: it is reported. The stop and
// the marker are re-asserted because both are no-ops on a VM that is genuinely
// asleep and both are the repair when it is not, and the park is re-asserted
// because it is the one that is missing in the case worth recovering.
func (manager *Manager) reassertSleep(
	ctx context.Context, runner *run.Runner, uuid string,
) (SleepResult, error) {
	commands := manager.commandsFor(runner)
	files := manager.filesFor(uuid)
	if err := manager.stopMarkAndPark(ctx, runner, uuid); err != nil {
		return SleepResult{}, err
	}
	if !manager.memorySnapshotMarkerPresent(ctx, commands, files) {
		return SleepResult{
			Reason: "already sleeping without a memory snapshot; the next wake is a cold boot",
		}, nil
	}
	size, err := manager.memorySnapshotSize(ctx, commands, files)
	return SleepResult{MemorySnapshot: true, MemorySnapshotBytes: size}, err
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

// sleepWithMemorySnapshot puts the VM to sleep on top of a complete snapshot and
// reports the memory file's size, which is the RAM this sleep actually freed.
//
// The size is read LAST, after the trap is armed, and that ordering is the
// difference between a bad measurement and an outage. A stat that fails, or a
// number that will not parse, would otherwise return with the VM stopped, marked
// sleeping and unparked — no counter, no rule, no route, no proxy-NDP — which is
// the state parkForWake exists to prevent, made unreachable by its own marker
// until an operator finds it. scripts/sleep-vm.py stats after it parks for
// exactly this reason.
func (manager *Manager) sleepWithMemorySnapshot(
	ctx context.Context, runner *run.Runner, uuid string,
) (SleepResult, error) {
	result := SleepResult{MemorySnapshot: true}
	if err := manager.stopMarkAndPark(ctx, runner, uuid); err != nil {
		return result, err
	}
	size, err := manager.memorySnapshotSize(ctx, manager.commandsFor(runner), manager.filesFor(uuid))
	result.MemorySnapshotBytes = size
	return result, err
}

// sleepWithoutMemorySnapshot is the fallback: no fast wake, but still a sleep.
// The stale marker goes first — a partial snapshot must never be restored — and
// the sleeping marker and the trap still land, so both the reboot suppression
// and the way back take effect either way.
func (manager *Manager) sleepWithoutMemorySnapshot(
	ctx context.Context, runner *run.Runner, uuid string, reason string,
) error {
	commands := manager.commandsFor(runner)
	files := manager.filesFor(uuid)
	if _, err := commands.Run(ctx, "sudo rm -f {}", files.memorySnapshotMarker); err != nil {
		return err
	}
	if err := manager.stopMarkAndPark(ctx, runner, uuid); err != nil {
		return fmt.Errorf("%w (falling back to a plain stop: %s)", err, reason)
	}
	return nil
}

// stopMarkAndPark is the order every path shares, and it is one function rather
// than three calls because the order is the correctness:
//
//   - the marker is written AFTER the stop, because a marker written first would
//     make the stop's own restart-on-failure a no-op and leave a VM marked
//     sleeping while it is still running;
//   - the trap is armed AFTER the marker, because the marker is what the trap
//     reads to decide a counted SYN is worth a wake, and because a VM is not
//     officially asleep until it carries one;
//   - and nothing may come between the marker and the trap, which is the failure
//     this shape forecloses: any step in there that can fail returns with the VM
//     asleep and unreachable.
func (manager *Manager) stopMarkAndPark(ctx context.Context, runner *run.Runner, uuid string) error {
	commands := manager.commandsFor(runner)
	files := manager.filesFor(uuid)
	if _, err := commands.Run(ctx, "sudo systemctl stop {}", files.unit); err != nil {
		return err
	}
	if _, err := commands.Run(ctx, "sudo touch {}", files.sleepingMarker); err != nil {
		return err
	}
	return manager.parkForWake(ctx, runner, uuid)
}

// memorySnapshotSize is the size of the memory file, which is the RAM the sleep
// gave back. A file that cannot be stat-ed is reported rather than guessed at:
// the VM is asleep either way, and a zero would read as a snapshot that freed
// nothing.
func (manager *Manager) memorySnapshotSize(
	ctx context.Context, commands commands, files virtualMachineFiles,
) (int64, error) {
	output, err := commands.Run(ctx, "sudo stat -c %s {}", files.memorySnapshotMemory)
	if err != nil {
		return 0, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(output), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("memory file size: %w", err)
	}
	return size, nil
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
