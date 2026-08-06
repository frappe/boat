package migration

import (
	"context"

	"github.com/frappe/boat/internal/paths"
)

// CleanupSourceParams carries the qemu-nbd pid ExportSource recorded, so cleanup can
// kill the export by pid first (falling back to the port's pidfile). The nbd port
// itself is DERIVED from the UUID.
//
// KeepAddress marks a keep-address (permanent-forward) migration, on which the source
// keeps forwarding the VM's /128 to the target and the vm-network-down teardown is
// SUPPRESSED so that forward path survives (spec/24 §2.9.4).
type CleanupSourceParams struct {
	NBDPID      int
	KeepAddress bool
}

// CleanupSource is the Cleanup phase: after the target VM is confirmed Running and
// the routes are re-pointed, tear the source copy down. It runs LAST and only after
// cutover, so destroying source state is safe. It kills the qemu-nbd export(s),
// lvremoves the transient -migrate snapshots, and runs the ordinary VM teardown
// (disable the unit, bring the network down, remove the jail tree and disk LVs) —
// leaving the source host as if the VM had never been here.
//
// networkDown is boat's own vm-network-down for this VM, injected so this phase stays
// host-free in tests; the legacy scripts shelled a Python vm-network-down, which on a
// boat host becomes this. Idempotent + best-effort on the teardown pokes: a re-entry
// after a partial cleanup finishes the rest. There is no orphan-LV reconciler behind
// this — the migration row IS the backstop. Ports scripts/migration-cleanup-source.py.
func CleanupSource(ctx context.Context, cmd commands, uuid string, params CleanupSourceParams, networkDown func(ctx context.Context) error) error {
	port, err := NBDPort(uuid)
	if err != nil {
		return err
	}
	virtualMachine := paths.ForVirtualMachine(uuid)

	// 1. Kill the NBD export(s): root by recorded pid + its pidfile, then data (+1) and
	//    the local-image base ship's base LV (+2) and image-dir tar (+3). Harmless
	//    no-ops when this migration shipped no base. The base LV itself is immutable
	//    and never removed.
	killNBD(ctx, cmd, params.NBDPID, port)
	for offset := 1; offset <= 3; offset++ {
		killNBD(ctx, cmd, 0, port+offset)
	}
	// The staged image-dir tars — a glob needs a shell, and the literal is ours (no
	// interpolation). Best-effort: a leftover tar is cosmetic, and the row re-enters.
	if _, err := cmd.Shell(ctx, "sudo rm -f "+runDirectory+"/migrate-base-meta-*.tar"); err != nil {
		_ = err
	}

	// 2. Remove the transient migration snapshots (guarded; no-op if already gone).
	if err := removeLV(ctx, cmd, rootSnapLV(uuid)); err != nil {
		return err
	}
	if err := removeLV(ctx, cmd, dataSnapLV(uuid)); err != nil {
		return err
	}

	// 3. Tear down the stale source VM — the terminate teardown, verbatim in shape.
	//    Best-effort pokes: the unit may already be gone.
	cmd.RunUnchecked(ctx, "sudo systemctl disable --now {}", virtualMachine.SystemdUnit())
	// On a keep-address (permanent-forward) migration the source keeps forwarding the
	// VM's /128 to the target — the mig6 tunnel, its route, the nft forward and the
	// proxy-NDP re-assert carry live tenant ingress until the block drains (spec/24
	// §2.9.4). vm-network-down would tear exactly that down and black-hole the tenant,
	// so it is SUPPRESSED here; the disk/nbd/snapshot teardown around it still runs. A
	// change-address migration brings the network down, the same teardown terminate does.
	if !params.KeepAddress && cmd.OK(ctx, "sudo test -f {}", virtualMachine.NetworkEnvironment()) {
		// Best-effort, like the Python's check=False: a network-down that could not
		// finish must not strand the rest of cleanup, and the row is the backstop.
		if err := networkDown(ctx); err != nil {
			_ = err
		}
	}
	cmd.RunUnchecked(ctx, "sudo rm -rf {}", virtualMachine.Directory())

	// 4. Remove the source disk LV(s) — checked, guarded by presence, so a re-entry is
	//    clean and a disk that will not remove surfaces rather than being swallowed.
	if err := removeLV(ctx, cmd, vmDiskLV(uuid)); err != nil {
		return err
	}
	return removeLV(ctx, cmd, dataDiskLV(uuid))
}
