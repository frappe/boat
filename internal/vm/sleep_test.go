package vm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const testFirecrackerUID = 1200

type sleepCommandSet struct {
	wakeTrap          string
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
}

func sleepCommands() sleepCommandSet {
	files := testFiles(testUUID)
	socketArgument := "socket=" + files.apiSocketDirectory + "/firecracker.socket body="
	return sleepCommandSet{
		wakeTrap:          "systemctl is-active --quiet atlas-wake-trap.service",
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
	}
}

// aHostWithRoom answers the two measurements the preflight makes: a 1 GiB guest
// on a host with 8 GB free.
func aHostWithRoom(fake *fakeCommands, commands sleepCommandSet) {
	fake.output(commands.guestMemory, "1024\n")
	fake.output(commands.freeSpace, "Avail\n8000000000\n")
	fake.output(commands.memorySize, "1073741824\n")
}

// The precondition the whole verb hangs on. A VM that sleeps with nothing
// watching its counter never wakes: it is parked, so it answers nothing, and it
// stays dark until an operator clicks Start. Refusing leaves the VM running,
// which is strictly better, so nothing after this check may run.
func TestSleepRefusesWhenTheWakeTrapIsNotRunning(t *testing.T) {
	commands := sleepCommands()
	fake := newFakeCommands()
	fake.reply(commands.wakeTrap, false)

	result, err := newTestManager(fake).Sleep(
		context.Background(), nil, testUUID, SleepRequest{FirecrackerUID: testFirecrackerUID},
	)

	if err == nil {
		t.Fatal("Sleep succeeded with no wake trap, want a refusal")
	}
	if !strings.Contains(err.Error(), "atlas-wake-trap.service") {
		t.Errorf("the refusal must name the unit: %v", err)
	}
	if result.MemorySnapshot {
		t.Error("a refused sleep reported a memory snapshot")
	}
	assertTrace(t, fake, "? "+commands.wakeTrap)
}

func TestSleepCapturesAMemorySnapshotThenStopsAndMarks(t *testing.T) {
	commands := sleepCommands()
	fake := newFakeCommands()
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
		"? "+commands.wakeTrap,
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
		commands.memorySize,
		commands.park,
	)
}

// The sleeping marker is what suppresses the unit's auto-start after a host
// reboot, so it is written after the stop on every path — including the ones
// that gave up on the snapshot. A VM left stopped but unmarked comes back on
// the next reboot with nobody having asked for it.
func TestSleepStillMarksSleepingWhenTheLauncherPredatesSnapshots(t *testing.T) {
	commands := sleepCommands()
	fake := newFakeCommands()
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
		"? "+commands.wakeTrap,
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
		"? "+commands.wakeTrap,
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
		"? "+commands.wakeTrap,
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

// The marker comes off BEFORE the start: the unit's ConditionPathNotExists sees
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
