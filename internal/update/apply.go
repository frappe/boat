package update

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
)

// Host is the daemon-side of an update — the three effects Apply cannot express as
// a subprocess and must ask the running daemon to perform. It is an interface so
// the sequencing below is tested with no host, and so the real implementation
// (which touches the journal, systemd and the Firecracker re-attach) lives with the
// daemon rather than here.
type Host interface {
	// Quiesce is §5 step 3: refuse new operations and checkpoint in-flight ones
	// into the journal, so an update interrupted mid-restart is replayable rather
	// than lost. It runs before the swap is made live by a restart.
	Quiesce(ctx context.Context) error
	// Resume undoes Quiesce. It is the abort path only: once RestartAndReattach has
	// run, a FRESH daemon is serving and there is nothing quiesced to resume, so
	// Resume is called just when Apply gives up before the restart.
	Resume(ctx context.Context) error
	// RestartAndReattach is §5 step 4: restart the units in a defined order and
	// RE-ATTACH to running Firecrackers rather than restart them (§3.3), so live
	// guests keep running and sleeping VMs stay asleep across the swap.
	RestartAndReattach(ctx context.Context) error
	// Healthy is §5 step 5: a GET /export round-trip plus unit liveness against the
	// just-swapped binary. A non-nil error triggers the rollback to N-1.
	Healthy(ctx context.Context) error
}

// Apply runs a verified release through §5's steps once the desired-versus-running
// decision (ShouldApply) has already chosen to update. The order is the spec's:
// verify, quiesce, atomically swap keeping N-1, restart-and-reattach, health-check,
// and roll back to N-1 on any failure after the swap.
//
// A failure BEFORE the swap leaves the host on the old binary untouched, so Apply
// only resumes the quiesced daemon and returns — there is nothing to roll back. A
// failure AFTER the swap is the case rollback exists for: N-1 is renamed back and
// the units restarted onto it, so the host ends on the version it started on rather
// than on a binary that could not come up.
func Apply(
	ctx context.Context,
	cmd commands,
	host Host,
	release Release,
	trusted ed25519.PublicKey,
) error {
	if err := Verify(release, trusted); err != nil { // step 2: verify FIRST, or nothing else
		return fmt.Errorf("update refused: %w", err)
	}
	if err := host.Quiesce(ctx); err != nil { // step 3: quiesce so the swap is replayable
		return fmt.Errorf("update quiesce: %w", err)
	}
	if err := Install(ctx, cmd, release.Binary); err != nil { // step 2: atomic swap, keep N-1
		// Nothing was swapped — the host is still on the old binary. Resume and
		// report; a rollback here would restore a binary that is already live.
		return resumeAfter(ctx, host, fmt.Errorf("update install: %w", err))
	}
	if err := host.RestartAndReattach(ctx); err != nil { // step 4
		return rollback(ctx, cmd, host, fmt.Errorf("update restart: %w", err))
	}
	if err := host.Healthy(ctx); err != nil { // step 5: health-check → roll back on failure
		return rollback(ctx, cmd, host, fmt.Errorf("update health check: %w", err))
	}
	return nil
}

// rollback restores N-1 and restarts onto it, then reports the original cause. A
// rollback that itself fails is the worst case — the host may be on a binary that
// will not come up — so both failures are joined and surfaced, never swallowed.
func rollback(ctx context.Context, cmd commands, host Host, cause error) error {
	if err := Rollback(ctx, cmd); err != nil {
		return errors.Join(cause, fmt.Errorf("rollback to N-1 also failed: %w", err))
	}
	if err := host.RestartAndReattach(ctx); err != nil {
		return errors.Join(cause, fmt.Errorf("restart onto N-1 also failed: %w", err))
	}
	return fmt.Errorf("update rolled back to N-1: %w", cause)
}

// resumeAfter un-quiesces after an abort that never swapped, folding a failed
// resume into the reported cause rather than hiding it.
func resumeAfter(ctx context.Context, host Host, cause error) error {
	if err := host.Resume(ctx); err != nil {
		return errors.Join(cause, fmt.Errorf("resume after aborted update also failed: %w", err))
	}
	return cause
}
