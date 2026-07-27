package run

import (
	"context"
	"errors"
	"os"
)

// InstallFile writes content to destination with mode, via `install -m <mode>
// <source> <destination>` — the create-or-replace-with-the-mode-set-in-one-shot
// semantics the shell heredocs relied on.
//
// The source is a REAL temp file, never /dev/stdin. uutils (rust-coreutils)
// install — the default on Ubuntu 26.04 — cannot reliably copy from a
// non-seekable source: feeding the content as the child's stdin and passing
// /dev/stdin fails about 90% of the time (measured 29 failures in 30 on a
// Self-Managed host) with "install: No such file or directory", while the very
// same pipe reads fine through cat. GNU install tolerates it, uutils does not,
// and the flakiness silently broke bootstrap and image sync. A spooled regular
// file is seekable, so install copies it 30 times out of 30.
func (runner *Runner) InstallFile(ctx context.Context, content string, destination string, mode string) error {
	source, err := spool(content)
	if err != nil {
		return err
	}
	// The spool is a 0600 file owned by this service; only the sudo'd install
	// reads it, and it is gone whether install succeeded or not.
	defer os.Remove(source)
	_, err = runner.Run(ctx, "sudo install -m {} {} {}", mode, source, destination)
	return err
}

// InstallDirectory creates destination with an explicit mode. Explicit because
// the per-VM directories are 0700 and owned by the VM's own uid — a directory
// left at the umask's mercy is a jail another VM can read.
func (runner *Runner) InstallDirectory(ctx context.Context, destination string, mode string) error {
	_, err := runner.Run(ctx, "sudo install -d -m {} {}", mode, destination)
	return err
}

// spool writes content to a regular temp file and returns its path. The file is
// removed again if it could not be written whole, so a truncated spool is never
// handed to install.
func spool(content string) (string, error) {
	file, err := os.CreateTemp("", "boat-install-")
	if err != nil {
		return "", err
	}
	_, writeError := file.WriteString(content)
	closeError := file.Close()
	if writeError != nil || closeError != nil {
		os.Remove(file.Name())
		return "", errors.Join(writeError, closeError)
	}
	return file.Name(), nil
}
