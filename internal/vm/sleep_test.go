package vm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const testFirecrackerUID = 1200

type sleepCommandSet struct {
	launcher          string
	socket            string
	dropSnapshot      string
	guestMemory       string
	freeSpace         string
	makeDirectory     string
	ownDirectory      string
	pause             string
	create            string
	vmstatePresent    string
	memoryPresent     string
	writeMarker       string
	removeMarker      string
	stop              string
	markSleeping      string
	park              string
	memorySize        string
	removeSleeping    string
	memorySnapshotSet string
	sleepingMarkerSet string
}

func sleepCommands() sleepCommandSet {
	files := testFiles(testUUID)
	socketArgument := "socket=" + files.apiSocketDirectory + "/firecracker.socket body="
	return sleepCommandSet{
		launcher:          "sudo grep -q snapshot/READY " + files.jailerLaunch,
		socket:            "sudo test -S " + files.apiSocket,
		dropSnapshot:      "sudo rm -rf " + files.memorySnapshotDirectory,
		guestMemory:       "sudo jq -r " + guestMemoryQuery + " " + files.firecrackerConfig,
		freeSpace:         "df --output=avail -B1 /var/lib/atlas",
		makeDirectory:     "install -d -m 0700 " + files.memorySnapshotDirectory,
		ownDirectory:      "sudo chown 1200:1200 " + files.memorySnapshotDirectory,
		pause:             "firecracker-api PATCH /vm " + socketArgument + pauseStateBody,
		create:            "firecracker-api PUT /snapshot/create " + socketArgument + memorySnapshotBody,
		vmstatePresent:    "sudo test -s " + files.memorySnapshotVMState,
		memoryPresent:     "sudo test -s " + files.memorySnapshotMemory,
		writeMarker:       "sudo touch " + files.memorySnapshotMarker,
		removeMarker:      "sudo rm -f " + files.memorySnapshotMarker,
		stop:              "sudo systemctl stop " + files.unit,
		markSleeping:      "sudo touch " + files.sleepingMarker,
		park:              "park " + testUUID,
		memorySize:        "sudo stat -c %s " + files.memorySnapshotMemory,
		removeSleeping:    "sudo rm -f " + files.sleepingMarker,
		memorySnapshotSet: "sudo test -f " + files.memorySnapshotMarker,
		sleepingMarkerSet: "sudo test -f " + files.sleepingMarker,
	}
}

// aHostWithRoom answers the two measurements the preflight makes: a 1 GiB guest
// on a host with 8 GB free.
func aHostWithRoom(fake *fakeCommands, commands sleepCommandSet) {
	fake.output(commands.guestMemory, "1024\n")
	fake.output(commands.freeSpace, "Avail\n8000000000\n")
	fake.output(commands.memorySize, "1073741824\n")
}

// anAwakeVirtualMachine says this VM is not already asleep. It is spelled out in
// every scenario rather than left to the fake's default, because the first thing
// Sleep asks is exactly this question and the two answers take different paths.
func anAwakeVirtualMachine(fake *fakeCommands, commands sleepCommandSet) {
	fake.reply(commands.sleepingMarkerSet, false)
}

// The precondition the whole verb hangs on. A VM that sleeps with nothing
// watching its counter never wakes: it is parked, so it answers nothing, and it
// stays dark until an operator clicks Start. Refusing leaves the VM running,
// which is strictly better, so nothing after this check may run.
func TestSleepRefusesWhenTheWakeTrapIsNotRunning(t *testing.T) {
	fake := newFakeCommands()
	fake.wakeTrapStopped = true

	result, err := newTestManager(fake).Sleep(
		context.Background(), nil, testUUID, SleepRequest{FirecrackerUID: testFirecrackerUID},
	)

	if err == nil {
		t.Fatal("Sleep succeeded with no wake trap, want a refusal")
	}
	if !strings.Contains(err.Error(), "wake trap is not running") {
		t.Errorf("the refusal must say what is missing: %v", err)
	}
	if result.MemorySnapshot {
		t.Error("a refused sleep reported a memory snapshot")
	}
	// Nothing at all ran: the gate is a hard precondition, so a trap-less host
	// never gets as far as pausing vCPUs or stopping a unit.
	assertTrace(t, fake)
}

// The gate asks about Boat's OWN trap and not about the Python daemon's unit.
//
// That daemon stands down while boat.service is active and stays enabled and
// active while it does (scripts/atlas-wake-trap.py), so `systemctl is-active
// atlas-wake-trap.service` answers yes for a reflex that has stopped polling —
// and a host Boat bootstrapped has no such unit at all, so the same question
// refused every correct sleep. A gate that reaches systemd for this answer is the
// bug, whatever unit it names.
func TestSleepDoesNotAskSystemdAboutAWakeTrapUnit(t *testing.T) {
	commands := sleepCommands()
	fake := newFakeCommands()
	anAwakeVirtualMachine(fake, commands)
	aHostWithRoom(fake, commands)

	if _, err := newTestManager(fake).Sleep(
		context.Background(), nil, testUUID, SleepRequest{FirecrackerUID: testFirecrackerUID},
	); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	for _, line := range fake.trace {
		if strings.Contains(line, "wake-trap") || strings.Contains(line, "is-active") {
			t.Errorf("the sleep gate asked systemd about a unit: %s", line)
		}
	}
}

func TestSleepCapturesAMemorySnapshotThenStopsAndMarks(t *testing.T) {
	commands := sleepCommands()
	fake := newFakeCommands()
	anAwakeVirtualMachine(fake, commands)
	aHostWithRoom(fake, commands)

	result, err := newTestManager(fake).Sleep(
		context.Background(), nil, testUUID, SleepRequest{FirecrackerUID: testFirecrackerUID},
	)

	if err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if !result.MemorySnapshot || result.MemorySnapshotBytes != 1073741824 {
		t.Errorf("result = %+v, want a 1 GiB memory snapshot", result)
	}
	assertTrace(t, fake,
		"? "+commands.sleepingMarkerSet,
		"? "+commands.launcher,
		"? "+commands.socket,
		commands.dropSnapshot,
		commands.guestMemory,
		commands.freeSpace,
		commands.makeDirectory,
		commands.ownDirectory,
		commands.pause,
		commands.create,
		"? "+commands.vmstatePresent,
		"? "+commands.memoryPresent,
		commands.writeMarker,
		commands.stop,
		commands.markSleeping,
		commands.park,
		commands.memorySize,
	)
}

// The ordering defect this test exists to keep fixed. The size of the memory
// file is a MEASUREMENT — it is what the sleep freed — and measuring it before
// arming the trap put a fallible step between the VM losing its networking and
// getting the one thing that can bring it back. A stat that failed, or a number
// that would not parse, returned with the VM stopped, marked sleeping and
// unparked: no counter, no rule, no route, no proxy-NDP, and a marker that keeps
// its unit from coming back at the next reboot. scripts/sleep-vm.py parks first
// for this reason.
func TestSleepArmsTheWakeTrapBeforeItMeasuresTheMemoryFile(t *testing.T) {
	commands := sleepCommands()
	fake := newFakeCommands()
	anAwakeVirtualMachine(fake, commands)
	aHostWithRoom(fake, commands)
	// The measurement fails, which must not cost the VM its way back.
	fake.reply(commands.memorySize, false)

	result, err := newTestManager(fake).Sleep(
		context.Background(), nil, testUUID, SleepRequest{FirecrackerUID: testFirecrackerUID},
	)

	if err == nil {
		t.Fatal("Sleep hid a measurement it could not make")
	}
	if !result.MemorySnapshot {
		t.Error("the snapshot that WAS taken went unreported because its size could not be read")
	}
	if countTrace(fake, commands.park) != 1 {
		t.Fatalf("the VM was left stopped and untrapped:\n  %s", strings.Join(fake.trace, "\n  "))
	}
	if indexOfTrace(t, fake, commands.park) > indexOfTrace(t, fake, commands.memorySize) {
		t.Error("the wake trap was armed after the measurement, so a failed stat strands the VM")
	}
}

// The sleeping marker is what suppresses the unit's auto-start after a host
// reboot, so it is written after the stop on every path — including the ones
// that gave up on the snapshot. A VM left stopped but unmarked comes back on
// the next reboot with nobody having asked for it.
func TestSleepStillMarksSleepingWhenTheLauncherPredatesSnapshots(t *testing.T) {
	commands := sleepCommands()
	fake := newFakeCommands()
	anAwakeVirtualMachine(fake, commands)
	fake.reply(commands.launcher, false)

	result, err := newTestManager(fake).Sleep(
		context.Background(), nil, testUUID, SleepRequest{FirecrackerUID: testFirecrackerUID},
	)

	if err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if result.MemorySnapshot || !strings.Contains(result.Reason, "re-provision") {
		t.Errorf("result = %+v, want a plain sleep naming the fix", result)
	}
	assertTrace(t, fake,
		"? "+commands.sleepingMarkerSet,
		"? "+commands.launcher,
		commands.removeMarker,
		commands.stop,
		commands.markSleeping,
		commands.park,
	)
}

func TestSleepFallsBackWhenTheHostIsShortOfSpace(t *testing.T) {
	commands := sleepCommands()
	fake := newFakeCommands()
	anAwakeVirtualMachine(fake, commands)
	aHostWithRoom(fake, commands)
	// A 1 GiB guest needs its RAM plus the margin; 64 MB is not that.
	fake.output(commands.freeSpace, "Avail\n64000000\n")

	result, err := newTestManager(fake).Sleep(
		context.Background(), nil, testUUID, SleepRequest{FirecrackerUID: testFirecrackerUID},
	)

	if err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if result.MemorySnapshot || !strings.Contains(result.Reason, "not enough free space") {
		t.Errorf("result = %+v, want a plain sleep with the space reason", result)
	}
	assertTrace(t, fake,
		"? "+commands.sleepingMarkerSet,
		"? "+commands.launcher,
		"? "+commands.socket,
		commands.dropSnapshot,
		commands.guestMemory,
		commands.freeSpace,
		commands.removeMarker,
		commands.stop,
		commands.markSleeping,
		commands.park,
	)
}

// The marker asserts a COMPLETE pair. A memory file that never landed must not
// get one, or the next start loads it and fails.
func TestSleepFallsBackWhenTheMemoryFileIsEmpty(t *testing.T) {
	commands := sleepCommands()
	fake := newFakeCommands()
	anAwakeVirtualMachine(fake, commands)
	aHostWithRoom(fake, commands)
	fake.reply(commands.memoryPresent, false)

	result, err := newTestManager(fake).Sleep(
		context.Background(), nil, testUUID, SleepRequest{FirecrackerUID: testFirecrackerUID},
	)

	if err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if result.MemorySnapshot || !strings.Contains(result.Reason, "snapshot failed") {
		t.Errorf("result = %+v, want a plain sleep with the snapshot failure", result)
	}
	if countTrace(fake, commands.writeMarker) != 0 {
		t.Error("wrote the READY marker over an incomplete snapshot")
	}
	assertTrace(t, fake,
		"? "+commands.sleepingMarkerSet,
		"? "+commands.launcher,
		"? "+commands.socket,
		commands.dropSnapshot,
		commands.guestMemory,
		commands.freeSpace,
		commands.makeDirectory,
		commands.ownDirectory,
		commands.pause,
		commands.create,
		"? "+commands.vmstatePresent,
		"? "+commands.memoryPresent,
		commands.removeMarker,
		commands.stop,
		commands.markSleeping,
		commands.park,
	)
}

// A replay must not destroy the snapshot it is replaying over. The fresh-sleep
// path drops the snapshot directory before it rewrites it, guarded on a `test
// -S` against a socket INODE that outlives the Firecracker that bound it — so a
// second sleep could pass that guard, delete a perfectly good memory image, fail
// to talk to the dead socket, and report Success. The VM cold-boots on its next
// wake with a green Task beside it.
func TestSleepingAnAlreadySleepingVirtualMachineKeepsItsMemorySnapshot(t *testing.T) {
	commands := sleepCommands()
	fake := newFakeCommands()
	fake.reply(commands.sleepingMarkerSet, true)
	fake.reply(commands.memorySnapshotSet, true)
	aHostWithRoom(fake, commands)

	result, err := newTestManager(fake).Sleep(
		context.Background(), nil, testUUID, SleepRequest{FirecrackerUID: testFirecrackerUID},
	)

	if err != nil {
		t.Fatalf("Sleep replayed on a sleeping VM: %v", err)
	}
	if !result.MemorySnapshot || result.MemorySnapshotBytes != 1073741824 {
		t.Errorf("result = %+v, want the snapshot the host already holds", result)
	}
	assertNotIssued(t, fake, commands.dropSnapshot)
	assertNotIssued(t, fake, commands.removeMarker)
	// Nothing is re-taken either: the guest is not there to be paused.
	assertNotIssued(t, fake, commands.pause)
	assertNotIssued(t, fake, commands.create)
}

// The case §11.5's crash recovery is built on. A daemon that died between
// writing the marker and arming the trap leaves a VM that is asleep and
// unreachable — the state parkForWake calls fatal — and the designed recovery is
// that Atlas retries the operation, which replays this verb. So the replay
// re-asserts all three: an inactive unit is stopped again for nothing, the marker
// is touched again for nothing, and the trap that was missing is armed.
func TestSleepingAnAlreadySleepingVirtualMachineReArmsTheWakeTrap(t *testing.T) {
	commands := sleepCommands()
	fake := newFakeCommands()
	fake.reply(commands.sleepingMarkerSet, true)
	fake.reply(commands.memorySnapshotSet, true)
	aHostWithRoom(fake, commands)

	if _, err := newTestManager(fake).Sleep(
		context.Background(), nil, testUUID, SleepRequest{FirecrackerUID: testFirecrackerUID},
	); err != nil {
		t.Fatalf("Sleep replayed on a sleeping VM: %v", err)
	}
	assertTrace(t, fake,
		"? "+commands.sleepingMarkerSet,
		commands.stop,
		commands.markSleeping,
		commands.park,
		"? "+commands.memorySnapshotSet,
		commands.memorySize,
	)
}

// A VM that slept without a snapshot is replayed without acquiring one: the
// guest whose RAM would be captured is not running. The reason says so, because
// the reason is the only record of why the next wake will be a cold boot.
func TestSleepingAnAlreadySleepingVirtualMachineReportsAColdWake(t *testing.T) {
	commands := sleepCommands()
	fake := newFakeCommands()
	fake.reply(commands.sleepingMarkerSet, true)
	fake.reply(commands.memorySnapshotSet, false)

	result, err := newTestManager(fake).Sleep(
		context.Background(), nil, testUUID, SleepRequest{FirecrackerUID: testFirecrackerUID},
	)

	if err != nil {
		t.Fatalf("Sleep replayed on a sleeping VM: %v", err)
	}
	if result.MemorySnapshot || !strings.Contains(result.Reason, "cold boot") {
		t.Errorf("result = %+v, want a sleep reporting that the next wake is cold", result)
	}
	assertNotIssued(t, fake, commands.memorySize)
}

// A replay whose park fails is still a failure. The VM is asleep and untrapped
// either way, and reporting Success would close the one operation that could
// still fix it.
func TestSleepingAnAlreadySleepingVirtualMachineReportsATrapItCouldNotArm(t *testing.T) {
	commands := sleepCommands()
	fake := newFakeCommands()
	fake.reply(commands.sleepingMarkerSet, true)
	fake.parkError = errors.New("nft is not answering")

	_, err := newTestManager(fake).Sleep(
		context.Background(), nil, testUUID, SleepRequest{FirecrackerUID: testFirecrackerUID},
	)

	if err == nil {
		t.Fatal("a replayed sleep that could not arm the wake trap reported success")
	}
	if !strings.Contains(err.Error(), "parked for wake") {
		t.Errorf("got %q, want an error naming the unparked state", err)
	}
}

// The marker comes off BEFORE the start: the unit's ConditionPathExists=! condition sees
// it and silently declines to start, so a start with the marker still there
// reports success and leaves the VM down.
func TestWakeRemovesTheMarkerBeforeStartingTheUnit(t *testing.T) {
	commands := sleepCommands()
	files := testFiles(testUUID)
	fake := newFakeCommands()

	if err := newTestManager(fake).Wake(context.Background(), nil, testUUID); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	assertTrace(t, fake,
		commands.removeSleeping,
		"? "+commands.memorySnapshotSet,
		"sudo systemctl start "+files.unit,
		"sudo systemctl is-active "+files.unit,
	)
}

// The started unit's own ExecStartPre unparks the network, which is the only
// place it can happen in the right order. Unparking here would take the SYN
// trap down while the VM is still coming up, and the client's retransmit would
// arrive at a host with nothing left to catch it.
func TestWakeDoesNotUnparkTheNetworkItself(t *testing.T) {
	fake := newFakeCommands()

	if err := newTestManager(fake).Wake(context.Background(), nil, testUUID); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	for _, line := range fake.trace {
		if strings.Contains(line, "nft") || strings.Contains(line, "ip -6") {
			t.Errorf("wake touched the parked network itself: %s", line)
		}
	}
}

func TestWakeReportsAStartItCouldNotMake(t *testing.T) {
	files := testFiles(testUUID)
	fake := newFakeCommands()
	fake.reply("sudo systemctl start "+files.unit, false)

	if err := newTestManager(fake).Wake(context.Background(), nil, testUUID); err == nil {
		t.Fatal("Wake succeeded, want the failed start reported")
	}
}

// A VM that is stopped but not parked is unreachable and un-wakeable: its
// networking was torn down by the stop and nothing is watching for the SYN that
// would bring it back. Reporting that as a successful sleep would hide an
// outage behind a green Task, so the failure has to surface.
func TestSleepFailsWhenTheWakeTrapCannotBeArmed(t *testing.T) {
	fake := newFakeCommands()
	commands := sleepCommands()
	anAwakeVirtualMachine(fake, commands)
	aHostWithRoom(fake, commands)
	fake.parkError = errors.New("nft is not answering")

	_, err := newTestManager(fake).Sleep(context.Background(), nil, testUUID, SleepRequest{FirecrackerUID: 1200})

	if err == nil {
		t.Fatal("a sleep that could not arm the wake trap reported success")
	}
	if !strings.Contains(err.Error(), "parked for wake") {
		t.Errorf("got %q, want an error naming the unparked state", err)
	}
}

// assertNotIssued fails when a command ran that this scenario says must not.
func assertNotIssued(t *testing.T, fake *fakeCommands, command string) {
	t.Helper()
	if countTrace(fake, command) != 0 {
		t.Errorf("ran %q, want it not to:\n  %s", command, strings.Join(fake.trace, "\n  "))
	}
}
