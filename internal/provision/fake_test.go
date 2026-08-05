// The harness every test in this package runs on: a host described as the answers
// its commands give, plus a recorder of every command issued. Nothing here needs
// LVM, a jailer, a mount or root.
//
// Commands are spelled out as literal strings in each test rather than derived, so
// a template that drifts from the one scripts/provision-vm.py renders shows up as a
// failing golden rather than as a host that quietly builds the wrong jail. The
// idiom mirrors internal/snapshot/fake_test.go and internal/netapply/vmnetwork.

package provision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/run"
)

var errCommandFailed = errors.New("command failed")

// The UUID internal/snapshot and internal/migration use, so the suites line up.
const testUUID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

const (
	testDirectory  = "/var/lib/atlas/virtual-machines/" + testUUID
	testJailRoot   = testDirectory + "/jail/firecracker/" + testUUID + "/root"
	testDevice     = "/dev/atlas/atlas-vm-" + testUUID
	testDataDevice = "/dev/atlas/atlas-data-" + testUUID
	testImage      = "/var/lib/atlas/images/ubuntu-24.04"
)

// testParams is the input golden.py fed the Python to capture every expected
// artifact in this package's tests, field for field.
func testParams() Params {
	return Params{
		VirtualMachineName: testUUID,
		ImageName:          "ubuntu-24.04",
		KernelFilename:     "vmlinux-6.8",
		RootfsFilename:     "rootfs.ext4",
		VCPUs:              2,
		MemoryMB:           2048,
		DiskGB:             25,
		SSHPublicKey:       "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5 operator@atlas",
		MACAddress:         "06:00:01:02:03:04",
		TapDevice:          "atlas-3f2504e0",
		VirtualMachineIPv6: "2001:db8::2",
		IPv4HostCIDR:       "100.64.200.9/30",
		IPv4GuestCIDR:      "100.64.200.10/30",
		IPv4Gateway:        "100.64.200.9",
		FirecrackerUID:     255999,
		Namespace:          "atlas-3f2504e0ns",
		HostVeth:           "atlas-h3f2504",
		NamespaceVeth:      "atlas-n3f2504",
		CgroupArguments:    []string{"memory.max=2415919104", "memory.swap.max=0", "cpu.max=200000 100000"},
		// Spelled out because the dataclass's default is 1 and Go's zero value is 0,
		// which means the opposite: an unformatted raw disk. The CLI carries the
		// default (flags.number("data-disk-format", 1)); a caller building Params by
		// hand has to, and this is where that is on the page.
		DataDiskFormat: 1,
	}
}

// readyHost is a host that answers yes to everything an ordinary cold provision
// asks: the image is there, so is the VM directory, and every LV it activates
// comes up as a block device. Each test then adds the scenario it is about.
func readyHost() *fakeCommands {
	return newFakeCommands().
		exists("test -f "+testImage+"/rootfs.ext4").
		exists("test -d /var/lib/atlas/virtual-machines").
		exists("test -b "+testDevice).
		exists("test -b "+testDataDevice).
		output("sudo ls -1 /var/lib/atlas/virtual-machines", testUUID+"\n").
		output("lsblk -ndo MAJ:MIN "+testDevice, "252:5  \n").
		output("lsblk -ndo MAJ:MIN "+testDataDevice, "252:6  \n")
}

// fakeCommands answers rendered commands from a script and records every one. A
// recorded line carries a prefix for how much the command's failure mattered: "? "
// for a boolean gate, "- " for a discarded exit code, and nothing for a command
// whose failure aborts the verb. The install helpers and the identity callback
// record a synthetic line so a golden shows them in sequence too.
type fakeCommands struct {
	outputs       map[string]string
	present       map[string]bool
	failing       map[string]bool
	installedFile map[string]installedFile
	trace         []string
}

// installedFile is one InstallFile call's content and mode, kept so a test can
// assert the BYTES written as well as that the call happened.
type installedFile struct {
	content string
	mode    string
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{
		outputs:       map[string]string{},
		present:       map[string]bool{},
		failing:       map[string]bool{},
		installedFile: map[string]installedFile{},
	}
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
	return fake.outputs[command], nil
}

// OK defaults to false: an artifact exists in a scenario only because the scenario
// said so, so a probe nobody scripted reads as absent.
func (fake *fakeCommands) OK(_ context.Context, template string, parameters ...any) bool {
	command := render(template, parameters...)
	fake.record("? ", command)
	return fake.present[command]
}

func (fake *fakeCommands) InstallFile(_ context.Context, content, destination, mode string) error {
	fake.installedFile[destination] = installedFile{content: content, mode: mode}
	line := "install-file " + mode + " " + destination
	fake.record("", line)
	if fake.failing[line] {
		return errCommandFailed
	}
	return nil
}

func (fake *fakeCommands) InstallDirectory(_ context.Context, destination, mode string) error {
	line := "install-dir " + mode + " " + destination
	fake.record("", line)
	if fake.failing[line] {
		return errCommandFailed
	}
	return nil
}

// recordInject is the identity callback a test hands Provision: it records the
// device it was given, so a golden pins BOTH that the injection happened and where
// in the sequence it sat — after the disk exists, before the jail is built.
func (fake *fakeCommands) recordInject() func(context.Context, string) error {
	return func(_ context.Context, device string) error {
		fake.record("", "inject "+device)
		return nil
	}
}

// refuseInject is the callback for a path that must NOT write identity — a warm
// clone, whose disk may not be mutated under the frozen RAM, and a boot-on-clone,
// whose identity was already injected through the clone device.
func refuseInject(t *testing.T) func(context.Context, string) error {
	return func(_ context.Context, device string) error {
		t.Errorf("identity was injected through %s, and this path must not write to the disk", device)
		return nil
	}
}

// render is the production renderer, so a recorded command is exactly the string
// run.Substitute produces — each parameter shell-quoted — and can be compared to
// the Python's trace byte for byte.
func render(template string, parameters ...any) string {
	rendered, err := run.Substitute(template, parameters...)
	if err != nil {
		panic(err)
	}
	return rendered
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

func assertIssued(t *testing.T, fake *fakeCommands, fragment string) {
	t.Helper()
	for _, recorded := range fake.trace {
		if strings.Contains(recorded, fragment) {
			return
		}
	}
	t.Errorf("expected a command matching %q, issued:\n  %s", fragment, strings.Join(fake.trace, "\n  "))
}

func assertNotIssued(t *testing.T, fake *fakeCommands, fragment string) {
	t.Helper()
	for _, recorded := range fake.trace {
		if strings.Contains(recorded, fragment) {
			t.Errorf("issued %q, want it not to:\n  %s", fragment, strings.Join(fake.trace, "\n  "))
		}
	}
}

// assertFile compares a generated file byte for byte with what the Python wrote for
// the same input. A diff here is the whole point of the port.
func assertFile(t *testing.T, fake *fakeCommands, destination, mode, want string) {
	t.Helper()
	got, found := fake.installedFile[destination]
	if !found {
		t.Fatalf("nothing was installed at %s", destination)
	}
	if got.mode != mode {
		t.Errorf("%s installed with mode %s, want %s", destination, got.mode, mode)
	}
	assertText(t, got.content, want)
}

func assertText(t *testing.T, got, want string) {
	t.Helper()
	if got == want {
		return
	}
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(want, "\n")
	for index := range max(len(gotLines), len(wantLines)) {
		gotLine, wantLine := lineAt(gotLines, index), lineAt(wantLines, index)
		if gotLine != wantLine {
			t.Fatalf("line %d:\ngot:  %q\nwant: %q\n\nwhole:\n%s", index+1, gotLine, wantLine, got)
		}
	}
	t.Fatalf("got:\n%s\nwant:\n%s", got, want)
}

func lineAt(lines []string, index int) string {
	if index >= len(lines) {
		return fmt.Sprintf("<no line %d>", index+1)
	}
	return lines[index]
}
