// The harness every scenario runs on: a host described as the answers its commands
// give, plus a recorder of every command issued. Nothing here needs LVM, a mount or
// root. Commands are spelled out as literal strings rather than derived, so a
// template that drifts from the one scripts/regenerate-host-keys-vm.py renders shows
// up as a failing golden rather than as a VM that quietly keeps its old SSH identity.

package hostkeys

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/paths"
)

var errCommandFailed = errors.New("command failed")

// The UUID scripts/lib/atlas/test_park.py uses, so the suites line up.
const (
	testUUID     = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	testHostname = "atlas-3f2504e0"
	testMount    = "/tmp/atlas-mount-abc123"
)

func reference() string { return "atlas/atlas-vm-" + testUUID }
func device() string    { return "/dev/atlas/atlas-vm-" + testUUID }

// fakeCommands answers rendered commands from a script and records every one. A
// recorded line carries a prefix for how much a failure mattered: "? " for a
// boolean gate, "- " for a discarded exit code, "install " for a directory install,
// and nothing for a command whose failure aborts the verb.
type fakeCommands struct {
	outputs map[string]string
	present map[string]bool
	failing map[string]bool
	trace   []string
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{outputs: map[string]string{}, present: map[string]bool{}, failing: map[string]bool{}}
}

func (fake *fakeCommands) output(command, text string) *fakeCommands {
	fake.outputs[command] = text
	return fake
}
func (fake *fakeCommands) exists(command string) *fakeCommands {
	fake.present[command] = true
	return fake
}
func (fake *fakeCommands) fails(command string) *fakeCommands {
	fake.failing[command] = true
	return fake
}

func (fake *fakeCommands) record(prefix, command string) {
	fake.trace = append(fake.trace, prefix+command)
}

func (fake *fakeCommands) Run(_ context.Context, template string, parameters ...any) (string, error) {
	command := render(template, parameters...)
	fake.record("", command)
	if fake.failing[command] {
		return "", errCommandFailed
	}
	return fake.outputs[command], nil
}

func (fake *fakeCommands) RunUnchecked(_ context.Context, template string, parameters ...any) (string, error) {
	command := render(template, parameters...)
	fake.record("- ", command)
	if fake.failing[command] {
		return "", errCommandFailed
	}
	return fake.outputs[command], nil
}

func (fake *fakeCommands) OK(_ context.Context, template string, parameters ...any) bool {
	command := render(template, parameters...)
	fake.record("? ", command)
	return fake.present[command]
}

func (fake *fakeCommands) InstallDirectory(_ context.Context, destination, mode string) error {
	fake.record("", fmt.Sprintf("install -d -m %s %s", mode, destination))
	return nil
}

// render substitutes each {} with its parameter the way run.Render does, minus the
// shell quoting — every value here is a path, a device or a UUID, and an unquoted
// line is the one a reader compares to the Python by eye. It panics on an arity
// mismatch, catching a miscounted template for free.
func render(template string, parameters ...any) string {
	parts := strings.Split(template, "{}")
	if len(parts)-1 != len(parameters) {
		panic(fmt.Sprintf("%q: %d placeholders, %d parameters", template, len(parts)-1, len(parameters)))
	}
	var builder strings.Builder
	for index, part := range parts {
		builder.WriteString(part)
		if index < len(parameters) {
			fmt.Fprintf(&builder, "%v", parameters[index])
		}
	}
	return builder.String()
}

func assertTrace(t *testing.T, fake *fakeCommands, expected ...string) {
	t.Helper()
	if len(fake.trace) != len(expected) {
		t.Fatalf("command sequence:\ngot (%d):\n  %s\nwant (%d):\n  %s",
			len(fake.trace), strings.Join(fake.trace, "\n  "),
			len(expected), strings.Join(expected, "\n  "))
	}
	for index := range expected {
		if fake.trace[index] != expected[index] {
			t.Errorf("command %d:\ngot:  %s\nwant: %s", index, fake.trace[index], expected[index])
		}
	}
}

// The whole happy path: gate on the LV, drop any stale memory snapshot, activate,
// mount, replace the three key kinds, generate a fresh ed25519, unmount.
func TestRegenerateHostKeysHappyPath(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo lvs --noheadings "+reference()).
		exists("test -b "+device()).
		output("sudo mktemp -d /tmp/atlas-mount-XXXXXX", testMount+"\n")

	result, err := RegenerateHostKeysVM(context.Background(), fake, RegenerateHostKeysParams{VirtualMachine: testUUID})
	if err != nil {
		t.Fatalf("RegenerateHostKeysVM: %v", err)
	}
	if result.VirtualMachine != testUUID || result.Hostname != testHostname {
		t.Fatalf("result = %+v, want VM=%s hostname=%s", result, testUUID, testHostname)
	}

	snapshot := paths.ForVirtualMachine(testUUID).MemorySnapshotDirectory()
	ssh := testMount + "/etc/ssh"
	assertTrace(t, fake,
		"? sudo lvs --noheadings "+reference(),
		"sudo rm -rf "+snapshot,
		"sudo lvchange -ay -K "+reference(),
		"sudo udevadm settle",
		"? test -b "+device(),
		"sudo mktemp -d /tmp/atlas-mount-XXXXXX",
		"sudo mount "+device()+" "+testMount,
		"install -d -m 0755 "+ssh,
		"sudo rm -f "+ssh+"/ssh_host_rsa_key "+ssh+"/ssh_host_rsa_key.pub",
		"sudo rm -f "+ssh+"/ssh_host_ecdsa_key "+ssh+"/ssh_host_ecdsa_key.pub",
		"sudo rm -f "+ssh+"/ssh_host_ed25519_key "+ssh+"/ssh_host_ed25519_key.pub",
		"sudo ssh-keygen -q -t ed25519 -f "+ssh+"/ssh_host_ed25519_key -N  -C root@"+testHostname,
		"- sudo umount "+testMount,
		"- sudo rmdir "+testMount,
	)
}

// A missing disk LV is a loud refusal, not a silent no-op: there is nothing to
// rotate, and the memory snapshot / activate / mount steps never run.
func TestRegenerateHostKeysRefusesMissingDisk(t *testing.T) {
	fake := newFakeCommands() // LV gate absent → false
	if _, err := RegenerateHostKeysVM(context.Background(), fake, RegenerateHostKeysParams{VirtualMachine: testUUID}); err == nil {
		t.Fatal("expected an error when the disk LV is missing")
	}
	assertTrace(t, fake, "? sudo lvs --noheadings "+reference())
}

// A non-UUID name never becomes an LV reference or a path segment.
func TestRegenerateHostKeysRejectsNonUUID(t *testing.T) {
	fake := newFakeCommands()
	if _, err := RegenerateHostKeysVM(context.Background(), fake, RegenerateHostKeysParams{VirtualMachine: "../etc"}); err == nil {
		t.Fatal("expected a rejection of a non-UUID name")
	}
	if len(fake.trace) != 0 {
		t.Errorf("a non-UUID name still issued commands: %v", fake.trace)
	}
}

// When udev has not made the node yet, activate falls back to vgmknodes and, if it
// still never appears, fails loud — the mount is never attempted.
func TestActivateFallsBackToVgmknodesThenFailsLoud(t *testing.T) {
	fake := newFakeCommands() // test -b never present
	if err := activate(context.Background(), fake, reference(), device()); err == nil {
		t.Fatal("activate did not fail when the node never became a block device")
	}
	assertTrace(t, fake,
		"sudo lvchange -ay -K "+reference(),
		"sudo udevadm settle",
		"? test -b "+device(),
		"sudo vgmknodes atlas",
		"sudo udevadm settle",
		"? test -b "+device(),
	)
}

// The mount is torn down even when key regeneration fails partway.
func TestMountIsUnmountedOnFailure(t *testing.T) {
	ssh := testMount + "/etc/ssh"
	fake := newFakeCommands().
		exists("sudo lvs --noheadings "+reference()).
		exists("test -b "+device()).
		output("sudo mktemp -d /tmp/atlas-mount-XXXXXX", testMount+"\n").
		fails("sudo ssh-keygen -q -t ed25519 -f " + ssh + "/ssh_host_ed25519_key -N  -C root@" + testHostname)

	if _, err := RegenerateHostKeysVM(context.Background(), fake, RegenerateHostKeysParams{VirtualMachine: testUUID}); err == nil {
		t.Fatal("expected the ssh-keygen failure to surface")
	}
	last := fake.trace[len(fake.trace)-2:]
	if last[0] != "- sudo umount "+testMount || last[1] != "- sudo rmdir "+testMount {
		t.Errorf("mount was not torn down on failure; tail was:\n  %s", strings.Join(last, "\n  "))
	}
}
