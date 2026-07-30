package run

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestInstallFileWritesTheContentWithItsMode drives the real install(1) through
// a stand-in sudo, so the argv under test is the one a host would see.
func TestInstallFileWritesTheContentWithItsMode(t *testing.T) {
	directory := fakeCommands(t)
	record := filepath.Join(t.TempDir(), "sudo-argv")
	fakeCommand(t, directory, "sudo", recordingCommand(record)+"\nexec \"$@\"")
	destination := filepath.Join(t.TempDir(), "network.env")

	if err := spooling(t).InstallFile(context.Background(), "TAP_DEVICE=tap0\n", destination, "0640"); err != nil {
		t.Fatalf("InstallFile: %v", err)
	}

	content, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("reading the destination: %v", err)
	}
	if string(content) != "TAP_DEVICE=tap0\n" {
		t.Errorf("destination content = %q", content)
	}
	info, err := os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("destination mode = %v (%v), want 0640", info.Mode().Perm(), err)
	}
}

// TestInstallFileSpoolsToASeekableFile is the uutils regression guard. Feeding
// the content on stdin and naming /dev/stdin as the source fails about 90% of
// the time under rust-coreutils install, which is what broke bootstrap and
// image sync; the source must be a real regular file, and it must be gone
// afterwards.
func TestInstallFileSpoolsToASeekableFile(t *testing.T) {
	directory := fakeCommands(t)
	record := filepath.Join(t.TempDir(), "sudo-argv")
	fakeCommand(t, directory, "sudo", recordingCommand(record)+"\nexec \"$@\"")
	destination := filepath.Join(t.TempDir(), "spooled")

	if err := spooling(t).InstallFile(context.Background(), "content", destination, "0644"); err != nil {
		t.Fatalf("InstallFile: %v", err)
	}

	argv := recorded(t, record)[1:]
	if len(argv) != 5 || !slices.Equal(argv[:3], []string{"install", "-m", "0644"}) || argv[4] != destination {
		t.Fatalf("install argv = %q", argv)
	}
	source := argv[3]
	if strings.Contains(source, "/dev/stdin") {
		t.Fatal("install was handed /dev/stdin, which uutils cannot read from a pipe")
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Errorf("spool %s still exists after the install", source)
	}
}

func TestInstallFileReportsAFailedInstall(t *testing.T) {
	directory := fakeCommands(t)
	record := filepath.Join(t.TempDir(), "sudo-argv")
	fakeCommand(t, directory, "sudo", recordingCommand(record)+"\nexit 1")

	err := spooling(t).InstallFile(context.Background(), "content", "/etc/nowhere", "0644")
	var commandError *CommandError
	if !errors.As(err, &commandError) {
		t.Fatalf("InstallFile error = %v, want *CommandError", err)
	}
	if source := recorded(t, record)[1:][3]; !os.IsNotExist(statError(source)) {
		t.Errorf("spool %s survived a failed install", source)
	}
}

func TestInstallDirectoryCreatesWithAnExplicitMode(t *testing.T) {
	directory := fakeCommands(t)
	fakeCommand(t, directory, "sudo", "exec \"$@\"")
	destination := filepath.Join(t.TempDir(), "jail")

	if err := NewRunner(nil).InstallDirectory(context.Background(), destination, "0700"); err != nil {
		t.Fatalf("InstallDirectory: %v", err)
	}

	info, err := os.Stat(destination)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("destination = %v, want a 0700 directory", info.Mode())
	}
}

// spooling is a Runner whose spool is redirected into the test's temp directory:
// the real DefaultSpoolPath is /var/lib/boat/spool/install, which a test host
// neither has nor may create, and the spool being one fixed path is exactly what
// the sudoers install lines depend on — so a test names its own path rather than
// racing the real one.
func spooling(t *testing.T) *Runner {
	runner := NewRunner(nil)
	runner.spoolPath = filepath.Join(t.TempDir(), "spool", "install")
	return runner
}

func statError(path string) error {
	_, err := os.Stat(path)
	return err
}

// A symlink at the spool path must be refused, not followed. The sudoers lines
// pin the source PATH; they cannot pin its inode, and install(1) follows a link
// — so a daemon that could point its own spool at a root-only file would have
// that file's contents copied out under the sudoers line's mode. One of those
// modes is 0444. Demonstrated on a live host with a canary before this guard.
func TestSpoolRefusesToFollowASymlinkAtItsOwnPath(t *testing.T) {
	directory := t.TempDir()
	secret := filepath.Join(directory, "root-only")
	if err := os.WriteFile(secret, []byte("CANARY"), 0o600); err != nil {
		t.Fatalf("could not write the canary: %v", err)
	}
	spool := filepath.Join(directory, "spool", "install")
	if err := os.MkdirAll(filepath.Dir(spool), 0o700); err != nil {
		t.Fatalf("could not create the spool directory: %v", err)
	}
	if err := os.Symlink(secret, spool); err != nil {
		t.Fatalf("could not plant the symlink: %v", err)
	}
	runner := NewRunner(nil)
	runner.spoolPath = spool

	err := runner.spool("replacement")

	// Either outcome is safe, and both are asserted: the link is refused, or it
	// was unlinked and a fresh file created. What must never happen is the
	// canary's bytes surviving where install(1) would then read them.
	if content, readErr := os.ReadFile(secret); readErr != nil || string(content) != "CANARY" {
		t.Errorf("the canary was written through the link: %q (%v)", content, readErr)
	}
	if err == nil {
		written, readErr := os.ReadFile(spool)
		if readErr != nil {
			t.Fatalf("the spool is unreadable after a successful write: %v", readErr)
		}
		if string(written) != "replacement" {
			t.Errorf("the spool holds %q, want the content just written", written)
		}
		info, statErr := os.Lstat(spool)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 {
			t.Errorf("the spool is still a symlink after the write (%v)", statErr)
		}
	}
}
