package vm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/frappe/boat/internal/fcattach"
	"github.com/frappe/boat/internal/run"
)

// These tests assert the command sequence each verb emits, because on a machine
// with no Firecracker and no systemd that sequence is the whole of the
// behaviour — and it is exactly what a differential test against the Python
// originals compares. Nothing here touches internal/run or internal/paths: both
// are stubs today, and depending on another agent's half-finished package would
// make these tests measure the wrong thing.

// errCommandFailed stands in for run.CommandError, whose Error method is still
// a panicking stub. This package only ever propagates a command failure, so the
// concrete type never matters to it.
var errCommandFailed = errors.New("command failed")

const (
	testUUID = "11111111-2222-3333-4444-555555555555"
	// testFirecrackerPID is the pid a live probe reports, and is the pid in
	// internal/fcattach's own canned `ss` line — so a reader comparing the two
	// packages' tests is looking at one host.
	testFirecrackerPID = 15843
)

// testFiles spells out the slice of the path layout this package addresses a VM
// through, rather than deriving it, so the golden command lines below read like
// the lines an operator sees in a Task log.
func testFiles(uuid string) virtualMachineFiles {
	directory := "/var/lib/atlas/virtual-machines/" + uuid
	jailRoot := directory + "/jail/firecracker/" + uuid + "/root"
	return virtualMachineFiles{
		unit:                    "firecracker-vm@" + uuid + ".service",
		directory:               directory,
		networkEnvironment:      directory + "/network.env",
		jailRoot:                jailRoot,
		jailerLaunch:            directory + "/jailer-launch.sh",
		firecrackerConfig:       jailRoot + "/firecracker.json",
		rootFilesystemNode:      jailRoot + "/rootfs.ext4",
		dataNode:                jailRoot + "/data.ext4",
		memorySnapshotDirectory: jailRoot + "/snapshot",
		memorySnapshotMarker:    jailRoot + "/snapshot/READY",
		memorySnapshotVMState:   jailRoot + "/snapshot/vmstate.bin",
		memorySnapshotMemory:    jailRoot + "/snapshot/mem.bin",
		sleepingMarker:          directory + "/sleeping",
		apiSocket:               jailRoot + "/run/firecracker.socket",
		apiSocketDirectory:      jailRoot + "/run",
		apiSocketName:           "firecracker.socket",
	}
}

// fakeCommands records every rendered command and answers it from a script.
//
// A recorded line carries a prefix for how much the command's failure mattered:
// "? " for a boolean gate, "- " for a discarded exit code, nothing for a
// command whose failure aborts the verb. A sequence therefore shows not only
// what ran but which parts were best-effort — which is most of what these ports
// get wrong.
type fakeCommands struct {
	trace   []string
	replies map[string][]bool
	calls   map[string]int
	outputs map[string]string
	// parkError lets a test make arming the wake trap fail, which must fail the
	// sleep: a VM that is stopped and untrapped can never come back on its own.
	parkError error
	// liveness is what the Firecracker probe answers. It is a value rather than a
	// scripted command because the commands that probe renders belong to
	// internal/fcattach and are asserted there; spelling them again here would be
	// two copies of one contract, and only one of them would be updated.
	liveness fakeLiveness
	// wakeTrapStopped makes this host one whose wake reflex is not running, which
	// is the one state a sleep must refuse.
	wakeTrapStopped bool
}

// fakeLiveness is one answer from the liveness probe: a live Firecracker in a
// named guest state, nothing answering, or a probe that could not be made.
type fakeLiveness struct {
	process fcattach.Process
	live    bool
	err     error
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{
		replies: map[string][]bool{},
		calls:   map[string]int{},
		outputs: map[string]string{},
		// A live, running guest by default, for the same reason an unscripted
		// command succeeds by default: a scenario states what it took away from a
		// healthy host, and everything it did not mention is healthy.
		liveness: fakeLiveness{
			process: fcattach.Process{
				UUID: testUUID, Pid: testFirecrackerPID, State: fcattach.StateRunning,
			},
			live: true,
		},
	}
}

// reply scripts the successive answers to one rendered command. The last answer
// repeats, which is what the graceful stop's poll needs.
func (fake *fakeCommands) reply(command string, answers ...bool) {
	fake.replies[command] = answers
}

func (fake *fakeCommands) output(command string, text string) {
	fake.outputs[command] = text
}

func (fake *fakeCommands) succeeds(command string) bool {
	index := fake.calls[command]
	fake.calls[command]++
	answers := fake.replies[command]
	switch {
	case index < len(answers):
		return answers[index]
	case len(answers) > 0:
		return answers[len(answers)-1]
	default:
		return true
	}
}

func (fake *fakeCommands) record(prefix string, command string) {
	fake.trace = append(fake.trace, prefix+command)
}

func (fake *fakeCommands) Run(_ context.Context, template string, parameters ...any) (string, error) {
	command := render(template, parameters...)
	fake.record("", command)
	if !fake.succeeds(command) {
		return "", errCommandFailed
	}
	return fake.outputs[command], nil
}

func (fake *fakeCommands) RunUnchecked(
	_ context.Context, template string, parameters ...any,
) (string, error) {
	command := render(template, parameters...)
	fake.record("- ", command)
	fake.succeeds(command)
	return fake.outputs[command], nil
}

func (fake *fakeCommands) OK(_ context.Context, template string, parameters ...any) bool {
	command := render(template, parameters...)
	fake.record("? ", command)
	return fake.succeeds(command)
}

// Input records the command together with what was fed to its standard input,
// because for the two appends a rebuild makes the content IS the behaviour: a
// test that only checked the `tee -a` line would not notice the wrong bytes
// going into /etc/fstab.
func (fake *fakeCommands) Input(
	_ context.Context, stdin string, template string, parameters ...any,
) (string, error) {
	command := fmt.Sprintf("%s <<%q", render(template, parameters...), stdin)
	fake.record("", command)
	if !fake.succeeds(command) {
		return "", errCommandFailed
	}
	return fake.outputs[command], nil
}

func (fake *fakeCommands) InstallFile(
	_ context.Context, content string, destination string, mode string,
) error {
	command := fmt.Sprintf("install -m %s %q %s", mode, content, destination)
	fake.record("", command)
	if !fake.succeeds(command) {
		return errCommandFailed
	}
	return nil
}

func (fake *fakeCommands) InstallDirectory(
	_ context.Context, destination string, mode string,
) error {
	command := fmt.Sprintf("install -d -m %s %s", mode, destination)
	fake.record("", command)
	if !fake.succeeds(command) {
		return errCommandFailed
	}
	return nil
}

func (fake *fakeCommands) FirecrackerAPI(
	_ context.Context, socketDirectory, socketName, method, apiPath, body string,
) error {
	command := fmt.Sprintf(
		"firecracker-api %s %s socket=%s/%s body=%s", method, apiPath, socketDirectory, socketName, body,
	)
	fake.record("", command)
	if !fake.succeeds(command) {
		return errCommandFailed
	}
	return nil
}

// render substitutes each {} with its parameter the way run.Render does, minus
// the shell quoting — every value here is a path or a UUID, and an unquoted
// line is the one a reader can compare to the Python by eye. It panics on an
// arity mismatch, which catches a miscounted template for free.
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

// fakeClock advances only when something sleeps, so the thirty-second poll
// costs nothing while the deadline arithmetic under test stays the real one.
type fakeClock struct{ now time.Time }

func (clock *fakeClock) Now() time.Time { return clock.now }

func (clock *fakeClock) Sleep(duration time.Duration) { clock.now = clock.now.Add(duration) }

func newTestManager(fake *fakeCommands) *Manager {
	return &Manager{
		commandsFor: func(*run.Runner) commands { return fake },
		filesFor:    testFiles,
		clock:       &fakeClock{now: time.Unix(1700000000, 0).UTC()},
		// Recorded on the same trace as everything else, so a sleep that forgets
		// to arm the wake trap shows up as a missing line rather than as nothing.
		park: func(_ context.Context, _ *run.Runner, uuid string) error {
			fake.trace = append(fake.trace, "park "+uuid)
			return fake.parkError
		},
		// Traced too, so an observation that took the host's word for a running VM
		// is a missing line rather than an assertion nobody wrote.
		liveness: func(_ context.Context, _ *run.Runner, uuid string) (fcattach.Process, bool, error) {
			fake.trace = append(fake.trace, "liveness "+uuid)
			return fake.liveness.process, fake.liveness.live, fake.liveness.err
		},
		wakeTrapResident: func() bool { return !fake.wakeTrapStopped },
	}
}

func assertTrace(t *testing.T, fake *fakeCommands, expected ...string) {
	t.Helper()
	if len(fake.trace) != len(expected) {
		t.Fatalf("command sequence:\ngot:\n  %s\nwant:\n  %s",
			strings.Join(fake.trace, "\n  "), strings.Join(expected, "\n  "))
	}
	for index := range expected {
		if fake.trace[index] != expected[index] {
			t.Errorf("command %d:\ngot:  %s\nwant: %s", index, fake.trace[index], expected[index])
		}
	}
}

// The seams have exactly one real implementation each, and a nil one is not a
// test failure but a nil dereference inside a daemon supervising live VMs — on
// the first observation of a running VM, or on the first sleep.
func TestNewManagerIsWiredToTheHost(t *testing.T) {
	manager := NewManager()

	if manager.commandsFor == nil || manager.filesFor == nil || manager.clock == nil ||
		manager.park == nil || manager.liveness == nil || manager.wakeTrapResident == nil {
		t.Fatal("NewManager left a seam nil")
	}
	// False on a Manager nobody is running a trap beside, which is what this
	// process is: the answer is the host's, not this type's.
	if manager.wakeTrapResident() {
		t.Error("wakeTrapResident answered true with no wake trap running")
	}
}

func TestExistsAsksTheHostForTheVirtualMachineDirectory(t *testing.T) {
	fake := newFakeCommands()
	files := testFiles(testUUID)
	fake.reply("sudo test -d "+files.directory, true)

	if !newTestManager(fake).Exists(context.Background(), nil, testUUID) {
		t.Fatal("Exists = false, want true")
	}
	assertTrace(t, fake, "? sudo test -d "+files.directory)
}

func TestExistsIsFalseForAVirtualMachineThisHostDoesNotHave(t *testing.T) {
	fake := newFakeCommands()
	files := testFiles(testUUID)
	fake.reply("sudo test -d "+files.directory, false)

	if newTestManager(fake).Exists(context.Background(), nil, testUUID) {
		t.Fatal("Exists = true, want false")
	}
}
