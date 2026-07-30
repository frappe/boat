package run

import (
	"context"
	"os"
	"path/filepath"
	"sync"
)

// DefaultSpoolPath is the ONE file InstallFile stages content through, and its
// being a single fixed path rather than a random one is a privilege boundary,
// not tidiness. The source used to be os.CreateTemp("", "boat-install-"), which
// the sudoers install lines had to match with `/tmp/boat-install-*` — and a `*`
// in a sudoers argument matches spaces, so that wildcard could carry install's
// own `-t <dir>` flag and drop a daemon-written file into /etc/sudoers.d, which
// is root. install copies its first source before erroring on a missing tail, so
// the file lands even when the rest of the argv is junk. One literal path lets
// every install line name its source with no wildcard at all, and that hole
// closes.
//
// It lives inside the unit's StateDirectory: systemd creates /var/lib/boat 0700
// owned by the boat service user (systemd/boat.service, StateDirectory=boat), so
// boat can write it and nothing unprivileged can read what it stages.
const DefaultSpoolPath = "/var/lib/boat/spool/install"

// spoolMutex serializes the stage-and-install below, and it is the price of the
// fixed path. The random name it replaced was collision-free by construction —
// two concurrent operations got two files — whereas one path is shared by every
// operation in the process. Without this lock VM A's content could be overwritten
// by a concurrent VM B between the write and install's read of it, and A's guest
// would be handed B's file. Installs are rare (rebuild, resize, the stop drop-in)
// and brief, so a process-wide lock costs nothing measurable.
var spoolMutex sync.Mutex

// InstallFile writes content to destination with mode, via `install -m <mode>
// <source> <destination>` — the create-or-replace-with-the-mode-set-in-one-shot
// semantics the shell heredocs relied on.
//
// The source is a REAL, seekable temp file, never /dev/stdin. uutils
// (rust-coreutils) install — the default on Ubuntu 26.04 — cannot reliably copy
// from a non-seekable source: feeding the content as the child's stdin and
// passing /dev/stdin fails about 90% of the time (measured 29 failures in 30 on
// a Self-Managed host) with "install: No such file or directory", while the very
// same pipe reads fine through cat. GNU install tolerates it, uutils does not,
// and the flakiness silently broke bootstrap and image sync. A spooled regular
// file is seekable, so install copies it 30 times out of 30.
func (runner *Runner) InstallFile(ctx context.Context, content string, destination string, mode string) error {
	spoolMutex.Lock()
	defer spoolMutex.Unlock()
	if err := runner.spool(content); err != nil {
		return err
	}
	// The spool is a 0600 file owned by this service; only the sudo'd install
	// reads it, and it is gone whether install succeeded or not.
	defer os.Remove(runner.spoolPath)
	_, err := runner.Run(ctx, "sudo install -m {} {} {}", mode, runner.spoolPath, destination)
	return err
}

// InstallDirectory creates destination with an explicit mode. Explicit because
// the per-VM directories are 0700 and owned by the VM's own uid — a directory
// left at the umask's mercy is a jail another VM can read.
func (runner *Runner) InstallDirectory(ctx context.Context, destination string, mode string) error {
	_, err := runner.Run(ctx, "sudo install -d -m {} {}", mode, destination)
	return err
}

// spool writes content to the runner's fixed spool file, creating the spool
// directory under the StateDirectory the first time. 0600 because only the
// sudo'd install has any business reading a guest's identity bytes back, and a
// regular file so install(1) can seek it — the uutils reason InstallFile spells
// out. Held under spoolMutex by the one caller.
func (runner *Runner) spool(content string) error {
	if err := os.MkdirAll(filepath.Dir(runner.spoolPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(runner.spoolPath, []byte(content), 0o600)
}
