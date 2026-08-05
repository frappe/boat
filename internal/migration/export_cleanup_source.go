package migration

import "context"

// ExportCleanupSourceParams is scripts/export-cleanup-source.py's inputs: the
// image whose export is being torn down (it keys the staged tar's filename) and
// the base NBD port it was served on. The tar export sits on NBDPort+1.
//
// The port is GIVEN rather than derived, unlike every phase in this package: a
// standalone base-image export belongs to no VM, so there is no UUID to derive it
// from — the export row carries it.
type ExportCleanupSourceParams struct {
	ImageName string
	NBDPort   int
}

// ExportCleanupSource is the source side of a standalone base-image EXPORT
// (spec/08 § two origins; the standalone form of the migration base ship,
// spec/24 §5.1): once the target has hydrated and collapsed the base into its own
// local LV, the source's NBD exports come down.
//
// Base-ONLY, and that is what separates it from CleanupSource: there is no VM,
// no snapshot, and no unit or disk teardown. The base LV is the source's own
// immutable image and is NEVER removed — all that goes is the two qemu-nbd
// processes the export started (the rootfs LV on NBDPort, the image-directory tar
// on NBDPort+1) and the staged tar itself.
//
// Idempotent and best-effort throughout: a re-entry after a partial cleanup just
// finishes the rest, and killing an already-dead export is a no-op because its
// pidfile is already gone. Ports scripts/export-cleanup-source.py.
func ExportCleanupSource(ctx context.Context, cmd commands, params ExportCleanupSourceParams) error {
	// Killed by pidfile-per-port, the same mechanism CleanupSource uses, and with no
	// recorded pid: this verb is not the process that started the export, so the
	// pidfile is the only handle it has. Port 0 means the export never started, and
	// there is nothing to stop.
	if params.NBDPort != 0 {
		killNBD(ctx, cmd, 0, params.NBDPort)
		killNBD(ctx, cmd, 0, params.NBDPort+1) // the image-dir tar export
	}
	// Keyed by image name, so this removes exactly the file THIS image's export
	// staged and not a concurrent export's. Best-effort: a leftover tar is cosmetic
	// and the export row is the backstop that re-enters.
	_, err := cmd.RunUnchecked(ctx, "sudo rm -f {}", baseMetaTarPath(params.ImageName))
	return err
}
