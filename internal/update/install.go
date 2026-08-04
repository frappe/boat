package update

import (
	"context"
	"fmt"

	"github.com/frappe/boat/internal/run"
)

// commands is the subprocess seam Install and Rollback need — the run.Runner
// methods, narrowed so a test can record the exact sequence with no host. Install
// stages the binary through InstallFile (a real seekable spool file, the uutils
// reason run.InstallFile spells out) and moves it with Run.
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
	InstallFile(ctx context.Context, content string, destination string, mode string) error
}

var _ commands = (*run.Runner)(nil)

const (
	// binaryPath is the one artifact an update replaces; the sudoers `mv`/`ln`
	// lines name these three literals, so they are constants rather than derived.
	binaryPath   = "/usr/local/bin/boat"
	stagingPath  = "/usr/local/bin/boat.staging"
	previousPath = "/usr/local/bin/boat.previous" // N-1, kept on disk for Rollback
)

// Install is §5 step 2's second half: stage the verified binary beside the live
// one, keep the current binary as N-1, then ATOMICALLY rename the new one into
// place. Verify (release.go) must have passed first — Install trusts its input.
//
// The rename is why this is not `install` over the live path. `mv` on one
// filesystem is rename(2), so a process that execs /usr/local/bin/boat mid-update
// gets either the whole old inode or the whole new one, never a half-copied file;
// and the running daemon keeps its own open inode until it is restarted (step 4).
// Staging and target share /usr/local/bin precisely so the rename stays within one
// filesystem.
func Install(ctx context.Context, cmd commands, binary []byte) error {
	if err := cmd.InstallFile(ctx, string(binary), stagingPath, "0755"); err != nil {
		return fmt.Errorf("stage new boat: %w", err)
	}
	// Keep N-1 BEFORE the swap: hardlink the current binary aside. -f replaces a
	// prior .previous; the old inode survives as this link and as the running
	// process's open fd, so a rollback has something to name.
	if _, err := cmd.Run(ctx, "sudo ln -f {} {}", binaryPath, previousPath); err != nil {
		return fmt.Errorf("keep N-1: %w", err)
	}
	if _, err := cmd.Run(ctx, "sudo mv -f {} {}", stagingPath, binaryPath); err != nil {
		return fmt.Errorf("swap in new boat: %w", err)
	}
	return nil
}

// Rollback is §5 step 5's failure path: atomically restore N-1 over the live path.
// After it the host runs the version it ran before the update — the whole reason
// Install kept .previous. It is a no-op-safe last resort: a rollback with no
// .previous (an update that failed before the swap) returns the mv's error, which
// the caller logs rather than looping on.
func Rollback(ctx context.Context, cmd commands) error {
	if _, err := cmd.Run(ctx, "sudo mv -f {} {}", previousPath, binaryPath); err != nil {
		return fmt.Errorf("restore N-1: %w", err)
	}
	return nil
}
