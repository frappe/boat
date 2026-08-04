package migration

import (
	"errors"
	"fmt"
)

// The migration phases fail loud at the host boundary rather than degrading: a
// pool that cannot take a snapshot, a source disk that is not here, a tunnel that
// is not up. Atlas sees the failure and retries the idempotent phase; a degraded
// success would leave Atlas thinking a migration advanced when the host did not.

// errThinPoolTooFull refuses to snapshot (or hydrate onto) a pool that is almost
// full: the snapshot is free up front but its later CoW writes would stall.
var errThinPoolTooFull = errors.New("thin pool too full for migration; free space first")

// errSourceDiskMissing names the disk that is not on this host — almost always the
// wrong UUID or the wrong server, both worth hearing about loudly.
func errSourceDiskMissing(uuid string) error {
	return fmt.Errorf("VM disk %s not found on this host; is the UUID right and the VM here?", lvReference(vmDiskLV(uuid)))
}

// errTunnelDown is returned when a cutover route install runs before the forward
// tunnel it steers traffic onto exists — the phases must run in order.
func errTunnelDown(device string) error {
	return fmt.Errorf("forward tunnel %s is not up; run the forward-up phase first", device)
}

// errNBDClientMissing and errKernelModuleMissing are the target's migration-dep
// pre-flight failures: the deps ship at bootstrap, so their absence means an
// out-of-date host that must be re-bootstrapped before it can receive a migration.
var errNBDClientMissing = errors.New("nbd-client not installed on the target; re-bootstrap before migrating")

func errKernelModuleMissing(module string) error {
	return fmt.Errorf("kernel module %q unavailable; install linux-modules-extra and re-bootstrap before migrating", module)
}

// errBaseImageMissing and errImageDirectoryMissing name the image artifact the
// target is missing — the target cannot boot the migrated VM until Sync to Server
// (or a base-image ship) has placed the image's LV and directory here.
func errBaseImageMissing(image string) error {
	return fmt.Errorf("base image LV %s not on target; run Sync to Server (or ship the base) first", lvReference(baseImageLV(image)))
}

func errImageDirectoryMissing(image string) error {
	return fmt.Errorf("image directory %s missing on target; run Sync to Server (or ship the base) first", imageDirectory(image))
}

// errIdentityDeviceMissing is returned when inject-identity finds neither the clone
// nor the plain LV — the clone step has not run, so there is nothing to write into.
func errIdentityDeviceMissing(clone, plain string) error {
	return fmt.Errorf("neither clone %s nor plain LV %s present; run the clone-target phase first", clone, plain)
}

// errInvalidRole and errTargetNeedsSourceHost are the forward tunnel's argument
// guards: a tunnel end is a source (TCP listener) or a target (connector that needs
// the source's address to dial).
func errInvalidRole(role string) error {
	return fmt.Errorf("tunnel role must be \"source\" or \"target\", got %q", role)
}

var errTargetNeedsSourceHost = errors.New("target role requires a source host to dial")

// errSocatDeviceTimeout is returned when socat did not create its tun device in the
// window addressTunnel waits — the carrier failed to start.
func errSocatDeviceTimeout(device string) error {
	return fmt.Errorf("socat did not create tun device %s in time", device)
}

// errUnknownPhase rejects a base ship (or any phased RPC) asked for a phase it does
// not implement.
func errUnknownPhase(phase string) error {
	return fmt.Errorf("unknown phase %q (expected \"prepare\" or \"finalize\")", phase)
}

// errBaseNotHydrated refuses to collapse a base ship's clone before every region is
// local — collapsing early would strand un-copied blocks behind a torn-down NBD.
func errBaseNotHydrated(image, status string) error {
	return fmt.Errorf("base %s not fully hydrated yet; refusing to collapse (%q)", image, status)
}
