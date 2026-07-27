package run

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestFirecrackerAPIRunsOneShellLineUnderSudo pins the shape the socket
// workaround depends on: sudo receives exactly three arguments, the last being
// the whole cd-and-curl line as one token, so the cd and the curl share one
// root shell and therefore one working directory.
func TestFirecrackerAPIRunsOneShellLineUnderSudo(t *testing.T) {
	directory := fakeCommands(t)
	record := filepath.Join(t.TempDir(), "sudo-argv")
	fakeCommand(t, directory, "sudo", recordingCommand(record))

	err := NewRunner(nil).FirecrackerAPI(
		context.Background(), "/var/lib/atlas/vm/jail/run", "firecracker.socket", "PATCH", "/vm", `{"state":"Paused"}`,
	)
	if err != nil {
		t.Fatalf("FirecrackerAPI: %v", err)
	}

	argv := recorded(t, record)[1:]
	if len(argv) != 3 || argv[0] != "sh" || argv[1] != "-c" {
		t.Fatalf("sudo argv = %q, want one shell and one script", argv)
	}
	if !strings.HasPrefix(argv[2], "cd /var/lib/atlas/vm/jail/run && curl --fail ") {
		t.Errorf("shell line = %q", argv[2])
	}
}

// TestFirecrackerAPIAddressesTheSocketByItsShortName: the connect happens from
// inside the socket's directory with a short relative name, because the
// absolute path is past AF_UNIX's sun_path limit.
func TestFirecrackerAPIAddressesTheSocketByItsShortName(t *testing.T) {
	directory := fakeCommands(t)
	record := filepath.Join(t.TempDir(), "curl-argv")
	fakeCommand(t, directory, "sudo", "exec \"$@\"")
	fakeCommand(t, directory, "curl", recordingCommand(record))
	socketDirectory := t.TempDir()
	body := `{"snapshot_type": "Full", "snapshot_path": "snapshot/vmstate.bin"}`

	err := NewRunner(nil).FirecrackerAPI(
		context.Background(), socketDirectory, "firecracker.socket", "PUT", "/snapshot/create", body,
	)
	if err != nil {
		t.Fatalf("FirecrackerAPI: %v", err)
	}

	lines := recorded(t, record)
	if lines[0] != socketDirectory {
		t.Errorf("curl ran in %q, want the socket's own directory %q", lines[0], socketDirectory)
	}
	assertArgument(t, lines[1:], "--unix-socket", "firecracker.socket")
	assertArgument(t, lines[1:], "-X", "PUT")
	assertArgument(t, lines[1:], "-d", body)
	if !slices.Contains(lines, "--fail") {
		t.Error("curl ran without --fail, so a refused state change would look like success")
	}
	if !slices.Contains(lines, "http://localhost/snapshot/create") {
		t.Errorf("curl argv = %q, want the API path in the URL", lines[1:])
	}
}

func TestFirecrackerAPIReportsACurlFailure(t *testing.T) {
	directory := fakeCommands(t)
	fakeCommand(t, directory, "sudo", "exec \"$@\"")
	fakeCommand(t, directory, "curl", "exit 22")

	err := NewRunner(nil).FirecrackerAPI(
		context.Background(), t.TempDir(), "firecracker.socket", "PATCH", "/vm", `{"state":"Resumed"}`,
	)
	var commandError *CommandError
	if !errors.As(err, &commandError) {
		t.Fatalf("FirecrackerAPI error = %v, want *CommandError", err)
	}
}

// assertArgument checks that flag is followed by exactly value — one argv
// entry, braces, spaces and quotes intact.
func assertArgument(t *testing.T, argv []string, flag string, value string) {
	t.Helper()
	index := slices.Index(argv, flag)
	if index < 0 || index+1 >= len(argv) {
		t.Fatalf("argv %q carries no %s", argv, flag)
	}
	if argv[index+1] != value {
		t.Errorf("%s = %q, want %q as exactly one argument", flag, argv[index+1], value)
	}
}
