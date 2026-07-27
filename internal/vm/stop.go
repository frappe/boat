package vm

import (
	"context"
	"fmt"
	"time"

	"github.com/frappe/boat/internal/run"
)

const (
	// How long to wait for the guest to power itself off after a SendCtrlAltDel
	// before falling through to the unit stop. Long enough for an ordinary Linux
	// shutdown (filesystem sync plus unmount), short enough not to hang a stop on
	// a wedged guest — the unit stop is the hard backstop either way.
	gracefulShutdownTimeout = 30 * time.Second
	gracefulPollInterval    = 1 * time.Second

	// The guest's power button. On a systemd guest ctrl-alt-del.target maps to
	// poweroff.target, so this is a real shutdown rather than a reset.
	sendCtrlAltDelBody = `{"action_type": "SendCtrlAltDel"}`

	// A bounded drain is a runtime drop-in because TimeoutStopSec is a load-time
	// property: `systemctl set-property` cannot change it on a loaded unit
	// (host-verified). /run is tmpfs, so a drop-in that outlives its stop cannot
	// outlive a reboot — but it is removed below anyway, so that an ordinary
	// later stop of the same VM gets the unit's default drain back.
	runtimeUnitDirectory   = "/run/systemd/system"
	boundedDrainDropInName = "atlas-migration-faststop.conf"
	boundedDrainDropInMode = "0644"
)

// StopRequest is how a caller asks for a stop.
type StopRequest struct {
	// Forced skips asking the guest to power itself off. KillMode=mixed then
	// SIGKILLs the cgroup with the guest never told to halt, so whatever was
	// dirty in its page cache at that instant is lost. That is correct only for
	// a caller that discards the guest's RAM anyway, such as a migration
	// cold-stop, or captures the disk another way.
	//
	// The polarity is deliberate: this is the field the wire calls `graceful`,
	// inverted so that the zero value — StopRequest{} — is the SAFE stop. A
	// caller that forgets to think about it gets the guest's filesystems synced;
	// losing writes takes saying so.
	Forced bool
	// TimeoutSeconds bounds the graceful drain without skipping ExecStopPost.
	// Zero leaves systemd's default drain in place.
	TimeoutSeconds int
}

// Stop takes the VM's unit down. The networking teardown is not done here: it
// is the unit's ExecStopPost, which is why every path below stops the unit
// rather than killing the process.
func (manager *Manager) Stop(
	ctx context.Context, runner *run.Runner, uuid string, request StopRequest,
) error {
	commands := manager.commandsFor(runner)
	files := manager.filesFor(uuid)
	if !request.Forced {
		manager.shutDownGuest(ctx, commands, files)
	}
	if err := manager.stopUnit(ctx, commands, files, request.TimeoutSeconds); err != nil {
		return err
	}
	manager.convergeClone(ctx, commands, uuid)
	return nil
}

// shutDownGuest asks the guest to power itself off through the Firecracker API,
// then waits for the unit to go inactive — which is how we learn the guest
// synced, halted, and Firecracker exited.
//
// Best-effort from end to end. A missing socket means the VM is not running; a
// guest that ignores ctrl-alt-del simply times out; a refused API call means we
// asked politely and were declined. None of those are reasons to fail a stop,
// because the unit stop below runs ExecStopPost regardless and is the real
// mechanism. The only thing lost is the guest's chance to sync.
func (manager *Manager) shutDownGuest(
	ctx context.Context, commands commands, files virtualMachineFiles,
) {
	if !commands.OK(ctx, "sudo test -S {}", files.apiSocket) {
		return
	}
	err := commands.FirecrackerAPI(
		ctx, files.apiSocketDirectory, files.apiSocketName, "PUT", "/actions", sendCtrlAltDelBody,
	)
	if err != nil {
		return
	}
	manager.waitForUnitInactive(ctx, commands, files)
}

// waitForUnitInactive polls until the unit reports inactive or the budget runs
// out. Returning on the timeout is not a failure — the caller's stop follows.
func (manager *Manager) waitForUnitInactive(
	ctx context.Context, commands commands, files virtualMachineFiles,
) {
	deadline := manager.clock.Now().Add(gracefulShutdownTimeout)
	for manager.clock.Now().Before(deadline) {
		if !commands.OK(ctx, "systemctl is-active --quiet {}", files.unit) {
			return
		}
		manager.clock.Sleep(gracefulPollInterval)
	}
}

func (manager *Manager) stopUnit(
	ctx context.Context, commands commands, files virtualMachineFiles, timeoutSeconds int,
) error {
	if timeoutSeconds > 0 {
		return manager.stopUnitWithBoundedDrain(ctx, commands, files, timeoutSeconds)
	}
	_, err := commands.Run(ctx, "sudo systemctl stop {}", files.unit)
	return err
}

// stopUnitWithBoundedDrain stops the unit under a short TimeoutStopSec, so a
// migration does not pay for a full shutdown grace period it is going to throw
// away anyway.
//
// This is deliberately NOT `systemctl kill -SIGKILL`. A stop, even one that
// times out and ends in systemd SIGKILLing the cgroup, still runs ExecStopPost;
// a kill skips it. ExecStopPost is what tears down the netns, the veth pair and
// the proxy-NDP entry, and a host that keeps answering NDP for a /128 it no
// longer hosts collides with the keep-address migration that follows. Bounding
// the drain is a scheduling decision; skipping the teardown is a bug.
func (manager *Manager) stopUnitWithBoundedDrain(
	ctx context.Context, commands commands, files virtualMachineFiles, timeoutSeconds int,
) error {
	directory, file := boundedDrainDropIn(files.unit)
	if _, err := commands.Run(ctx, "sudo mkdir -p {}", directory); err != nil {
		return err
	}
	content := fmt.Sprintf("[Service]\nTimeoutStopSec=%ds\n", timeoutSeconds)
	if err := commands.InstallFile(ctx, content, file, boundedDrainDropInMode); err != nil {
		return err
	}
	if _, err := commands.Run(ctx, "sudo systemctl daemon-reload"); err != nil {
		return err
	}
	// Removed however the stop turns out: a drop-in left behind would silently
	// shorten the drain of the next ordinary stop of this VM.
	defer manager.removeBoundedDrainDropIn(ctx, commands, directory, file)
	_, err := commands.Run(ctx, "sudo systemctl stop {}", files.unit)
	return err
}

// removeBoundedDrainDropIn restores the unit's default drain. Every step is
// unchecked: this is cleanup, and failing the stop over it would report a
// failure for work that succeeded. The rmdir is expected to fail whenever
// something else has put a drop-in in the same directory.
func (manager *Manager) removeBoundedDrainDropIn(
	ctx context.Context, commands commands, directory string, file string,
) {
	commands.RunUnchecked(ctx, "sudo rm -f {}", file)
	commands.RunUnchecked(ctx, "sudo rmdir {}", directory)
	commands.RunUnchecked(ctx, "sudo systemctl daemon-reload")
}

func boundedDrainDropIn(unit string) (directory string, file string) {
	directory = fmt.Sprintf("%s/%s.d", runtimeUnitDirectory, unit)
	return directory, directory + "/" + boundedDrainDropInName
}

// convergeClone removes a leftover dm-clone device so the VM's disk converges
// back to the plain LV.
//
// A boot-then-hydrate migration runs the guest on a clone device that reads
// through to the source and is then collapsed to a linear map onto the plain
// LV. The clone lingers after the guest exits and holds the plain LV busy, so a
// later lvremove — a terminate or a rebuild — fails with "used by another
// device". Removing it is safe only now that the guest is stopped and its
// rootfs fd is released. A VM that never migrated has no clone and this is a
// pair of no-ops. Idempotent and best-effort, like everything after a stop.
func (manager *Manager) convergeClone(ctx context.Context, commands commands, uuid string) {
	for _, suffix := range []string{"", "-data"} {
		name := fmt.Sprintf("atlas-vm-%s%s-clone", uuid, suffix)
		if commands.OK(ctx, "sudo dmsetup info {}", name) {
			commands.RunUnchecked(ctx, "sudo dmsetup remove {}", name)
		}
	}
}
