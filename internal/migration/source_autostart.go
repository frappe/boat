package migration

import (
	"context"

	"github.com/frappe/boat/internal/paths"
)

// SourceAutostart is the source side's Pending-phase fence against split brain
// (spec/24 §3). It takes the source VM's systemd unit out of multi-user.target —
// or puts it back — so a source-host reboot mid-migration cannot cold-boot a
// second live copy of the guest.
//
// The migration's core invariant is that the source guest stays Stopped from
// Pending until Cleanup: that is what makes the target's read-through consistent
// and the rollback trivial, and `systemctl stop` alone does NOT give it.
// provision enables the unit, it carries [Install] WantedBy=multi-user.target,
// and its only condition is the SLEEPING marker — there is no migration
// condition. So until Cleanup's `disable --now` (the LAST phase, potentially
// hours later) a source-host reboot cold-boots a SECOND live copy of the guest:
// same UUID, same UUID-derived MAC and tap, same host keys, and on a keep-address
// migration the same public /128 answered by two hosts. Nothing is asked —
// systemd's multi-user.target.wants symlink starts it.
//
// `disable` (never `disable --now`, never `mask`) is deliberately the weakest
// thing that closes that hole: it removes the WantedBy symlink and touches
// nothing else, so a running unit keeps running and the spec/24 §3 rollback — an
// explicit `systemctl start` of the intact source VM — still works. A marker file
// plus a Condition would block that explicit start too, the failure mode sleepy
// VMs already paid for.
//
// enabled=false (what Pending sends) disables; enabled=true is the inverse an
// operator runs to hand an abandoned source copy back its reboot survival.
// Idempotent: disabling a disabled unit and enabling an enabled one are both
// systemd no-ops. Ports scripts/migration-source-autostart.py.
func SourceAutostart(ctx context.Context, cmd commands, uuid string, enabled bool) error {
	unit := paths.ForVirtualMachine(uuid).SystemdUnit()
	// Two literal templates, not `"sudo systemctl "+verb+" {}"`: internal/run's
	// trust model — and internal/allowlist's readability check — need the command
	// word to be a literal, since a template assembled from a value is one a value
	// can grow. Only the unit name varies, in its quoted hole (units.commandFor
	// draws the same division for the sibling-unit verbs).
	if enabled {
		_, err := cmd.Run(ctx, "sudo systemctl enable {}", unit)
		return err
	}
	_, err := cmd.Run(ctx, "sudo systemctl disable {}", unit)
	return err
}
