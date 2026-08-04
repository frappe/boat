package migration

import "context"

// InjectIdentity is the InjectingIdentity phase: pick the device to write the VM's
// identity through, then delegate the write. In boot-then-hydrate the identity must
// be injected THROUGH the dm-clone before the guest boots on it — the plain
// atlas-vm-<uuid> LV mounts BUSY under a live clone, and writes through the clone
// land on the dest and count toward hydration. If the clone is already gone (the
// disk converged to the plain LV, e.g. a collapse-forward retry after a stop), it
// falls back to the directly-mountable plain LV; both carry the same identity write.
//
// The mount + write is boat's shared identity injection (internal/vm), passed as
// `inject` so this phase renders only the migration-specific device selection and
// stays host-free in tests. That injection PRESERVES the disk's host keys by default
// — the disk moved wholesale, so its SSH identity must survive the move (clients'
// known_hosts) — which is exactly the Python's regenerate_host_keys=False contract.
// Idempotent: re-injecting rewrites identical files. Ports
// scripts/migration-inject-identity.py.
func InjectIdentity(ctx context.Context, cmd commands, uuid string, inject func(ctx context.Context, device string) error) error {
	if _, err := normalizeHex(uuid); err != nil {
		return err // reject a malformed UUID before it names a device
	}
	device := "/dev/mapper/" + vmCloneName(uuid)
	if !cmd.OK(ctx, "sudo dmsetup info {}", vmCloneName(uuid)) {
		plain := lvDevicePath(vmDiskLV(uuid))
		if !cmd.OK(ctx, "sudo test -b {}", plain) {
			return errIdentityDeviceMissing(device, plain)
		}
		device = plain
	}
	return inject(ctx, device)
}
