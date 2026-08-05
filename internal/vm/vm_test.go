package vm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/frappe/boat/internal/fcattach"
	"github.com/frappe/boat/internal/netapply/reservedip"
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
	// denied is the third answer a host gives: a question this daemon was not
	// allowed to PUT to it, mapped to the call from which the host stops answering
	// it. It is a separate map from replies because a bool has nowhere to put the
	// third answer — which is exactly why no test in this package could reach the
	// case that made a sleeping VM read Stopped, or the one that `rm -rf`ed a good
	// memory snapshot.
	denied map[string]int
	// parkError lets a test make arming the wake trap fail, which must fail the
	// sleep: a VM that is stopped and untrapped can never come back on its own.
	parkError error
	// retireError lets a test make the park teardown fail, which must fail the
	// terminate before it removes the sidecar naming the address to withdraw.
	retireError error
	// liveness is what the Firecracker probe answers. It is a value rather than a
	// scripted command because the commands that probe renders belong to
	// internal/fcattach and are asserted there; spelling them again here would be
	// two copies of one contract, and only one of them would be updated.
	liveness fakeLiveness
	// wakeTrapStopped makes this host one whose wake reflex is not running, which
	// is the one state a sleep must refuse.
	wakeTrapStopped bool
	// reservedDelivery and reservedError are what the injected reserved-IP apply
	// answers. Like liveness, the nft/ip sequence it renders is asserted in
	// internal/netapply/reservedip, so restating it here would be two copies of one
	// contract; this test asserts only that the verb read the sidecar, wrote the
	// durable flag, and dispatched with the guest and veth it read.
	reservedDelivery reservedip.Delivery
	reservedError    error
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
		denied:  map[string]int{},
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

// deny makes one command unaskable: the host neither confirms nor denies,
// because sudo would not run it. That is what a missing sudoers grant looks like
// against the 0700 root-owned VM tree this daemon reads as an unprivileged user.
func (fake *fakeCommands) deny(command string) {
	fake.denyFrom(command, 0)
}

// denyFrom stops the host answering a command from its call'th invocation
// onward, so a scenario can let one question be answered and then have the SAME
// question stop being answerable. A verb that asks twice and decides on the
// difference — a start, whose retry hangs on re-reading one marker — has a case
// here that no all-or-nothing denial reaches.
func (fake *fakeCommands) denyFrom(command string, call int) {
	fake.denied[command] = call
}

// unaskable reports whether the call'th run of a command is one the host would
// not answer.
func (fake *fakeCommands) unaskable(command string, call int) bool {
	from, ever := fake.denied[command]
	return ever && call >= from
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

// Probe answers in three values, so a scenario can say "denied" as well as "no".
func (fake *fakeCommands) Probe(
	_ context.Context, template string, parameters ...any,
) (run.Answer, error) {
	command := render(template, parameters...)
	fake.record("? ", command)
	call := fake.calls[command]
	// Scripted answers are consumed either way, so a denial does not shift the
	// sequence a poll's later answers were written against.
	answered := fake.succeeds(command)
	switch {
	case fake.unaskable(command, call):
		return run.Unknown, fmt.Errorf("could not run %s", command)
	case answered:
		return run.Yes, nil
	}
	return run.No, nil
}

// OK is spelled in terms of Probe exactly as run.Runner.OK is, so a test that
// denies a command sees the same collapse the daemon does at the sites where the
// collapse is still deliberate.
func (fake *fakeCommands) OK(ctx context.Context, template string, parameters ...any) bool {
	answer, _ := fake.Probe(ctx, template, parameters...)
	return answer == run.Yes
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
		// On the same trace for the same reason: a terminate that forgets to
		// withdraw the VM's parked networking is a missing line here, and on a host
		// it is a permanent DROP on every SYN to an address Atlas will re-allocate.
		retire: func(_ context.Context, _ *run.Runner, uuid string) error {
			fake.trace = append(fake.trace, "retire "+uuid)
			return fake.retireError
		},
		// Traced too, so an observation that took the host's word for a running VM
		// is a missing line rather than an assertion nobody wrote.
		liveness: func(_ context.Context, _ *run.Runner, uuid string) (fcattach.Process, bool, error) {
			fake.trace = append(fake.trace, "liveness "+uuid)
			return fake.liveness.process, fake.liveness.live, fake.liveness.err
		},
		wakeTrapResident: func() bool { return !fake.wakeTrapStopped },
		// On the same trace as everything else, carrying the guest, veth and reserved
		// IP the verb resolved, so a verb that dispatched a stale guest address shows
		// up as a wrong line rather than as a NAT built around the wrong VM.
		attachReservedIP: func(_ context.Context, _ *run.Runner, guestIPv4, hostVeth, reservedIPv4 string) (reservedip.Delivery, error) {
			fake.trace = append(fake.trace, "attach-reserved-ip "+guestIPv4+" "+hostVeth+" "+reservedIPv4)
			return fake.reservedDelivery, fake.reservedError
		},
		detachReservedIP: func(_ context.Context, _ *run.Runner, guestIPv4 string) error {
			fake.trace = append(fake.trace, "detach-reserved-ip "+guestIPv4)
			return fake.reservedError
		},
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
		manager.park == nil || manager.retire == nil || manager.liveness == nil ||
		manager.wakeTrapResident == nil {
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

// Only a PROVEN no is "this host does not have that VM".
//
// The caller turns false into a 404 naming this host and this UUID, and Atlas
// reads that as "the VM is somewhere else" — the one answer a control plane must
// never be handed by accident. The directory is 0700 and root-owned and this
// daemon is not root, so a missing sudoers line is the everyday way this probe
// fails, and it is a fact about the host rather than about the VM. The verb goes
// ahead instead and meets the same denial with a command that says what failed.
func TestExistsDoesNotDisownAVirtualMachineItCouldNotLookFor(t *testing.T) {
	fake := newFakeCommands()
	files := testFiles(testUUID)
	fake.deny("sudo test -d " + files.directory)

	if !newTestManager(fake).Exists(context.Background(), nil, testUUID) {
		t.Fatal("a directory this host could not read was reported as a VM this host does not have")
	}
}
