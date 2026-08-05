package vm

import (
	"context"
	"strings"
	"testing"
)

type stopCommandSet struct {
	socket       string
	powerOff     string
	poll         string
	stop         string
	dropInMkdir  string
	dropInWrite  string
	daemonReload string
	dropInRemove string
	dropInRmdir  string
	rootClone    string
	dataClone    string
}

func stopCommands() stopCommandSet {
	files := testFiles(testUUID)
	dropInDirectory := "/run/systemd/system/" + files.unit + ".d"
	dropInFile := dropInDirectory + "/atlas-migration-faststop.conf"
	return stopCommandSet{
		socket: "sudo test -S " + files.apiSocket,
		powerOff: "firecracker-api PUT /actions socket=" + files.apiSocketDirectory +
			"/firecracker.socket body=" + sendCtrlAltDelBody,
		poll:         "systemctl is-active --quiet " + files.unit,
		stop:         "sudo systemctl stop " + files.unit,
		dropInMkdir:  "sudo mkdir -p " + dropInDirectory,
		dropInWrite:  `install -m 0644 "[Service]\nTimeoutStopSec=5s\n" ` + dropInFile,
		daemonReload: "sudo systemctl daemon-reload",
		dropInRemove: "sudo rm -f " + dropInFile,
		dropInRmdir:  "sudo rmdir " + dropInDirectory,
		rootClone:    "sudo dmsetup info atlas-vm-" + testUUID + "-clone",
		dataClone:    "sudo dmsetup info atlas-vm-" + testUUID + "-data-clone",
	}
}

// noLeftoverClones is the ordinary case: the VM never migrated, so neither
// clone device exists and the convergence below is a pair of no-ops.
func noLeftoverClones(fake *fakeCommands, commands stopCommandSet) {
	fake.reply(commands.rootClone, false)
	fake.reply(commands.dataClone, false)
}

func TestStopGracefullyPowersTheGuestOffFirst(t *testing.T) {
	commands := stopCommands()
	fake := newFakeCommands()
	fake.reply(commands.socket, true)
	fake.reply(commands.poll, false)
	noLeftoverClones(fake, commands)

	err := newTestManager(fake).Stop(context.Background(), nil, testUUID, StopRequest{})

	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertTrace(t, fake,
		"? "+commands.socket,
		commands.powerOff,
		"? "+commands.poll,
		commands.stop,
		"? "+commands.rootClone,
		"? "+commands.dataClone,
	)
}

func TestStopSkipsTheGuestShutdownWhenTheSocketIsGone(t *testing.T) {
	commands := stopCommands()
	fake := newFakeCommands()
	fake.reply(commands.socket, false)
	noLeftoverClones(fake, commands)

	if err := newTestManager(fake).Stop(
		context.Background(), nil, testUUID, StopRequest{},
	); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertTrace(t, fake,
		"? "+commands.socket,
		commands.stop,
		"? "+commands.rootClone,
		"? "+commands.dataClone,
	)
}

// A guest that ignores ctrl-alt-del must not hang the stop: the poll gives up
// after its budget and the unit stop takes over.
func TestStopGivesUpOnAGuestThatNeverHalts(t *testing.T) {
	commands := stopCommands()
	fake := newFakeCommands()
	fake.reply(commands.socket, true)
	fake.reply(commands.poll, true)
	noLeftoverClones(fake, commands)

	if err := newTestManager(fake).Stop(
		context.Background(), nil, testUUID, StopRequest{},
	); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	polls := countTrace(fake, "? "+commands.poll)
	wantPolls := int(gracefulShutdownTimeout / gracefulPollInterval)
	if polls != wantPolls {
		t.Errorf("polled %d times, want %d", polls, wantPolls)
	}
	if fake.trace[len(fake.trace)-3] != commands.stop {
		t.Errorf("the unit stop did not follow the timeout: %v", fake.trace)
	}
}

// Only a PROVEN missing socket skips the guest's chance to sync.
//
// The whole graceful half of a stop is best-effort, which is exactly why the
// collapse is not free here: a probe read as "no socket" costs a guest its
// filesystem sync and then SIGKILLs its cgroup, and nothing about that is loud.
// A socket this daemon could not look at is asked anyway — the API call is itself
// tolerant, so the cost of being wrong that way is one refused connection.
func TestStopStillAsksTheGuestWhenItCouldNotLookAtTheSocket(t *testing.T) {
	commands := stopCommands()
	fake := newFakeCommands()
	fake.deny(commands.socket)
	fake.reply(commands.poll, false)
	noLeftoverClones(fake, commands)

	if err := newTestManager(fake).Stop(
		context.Background(), nil, testUUID, StopRequest{},
	); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertTrace(t, fake,
		"? "+commands.socket,
		commands.powerOff,
		"? "+commands.poll,
		commands.stop,
		"? "+commands.rootClone,
		"? "+commands.dataClone,
	)
}

// The drain's poll waits for a PROVEN inactive unit, and nothing else ends it
// early.
//
// `is-active --quiet` prints nothing and exits non-zero for a unit that is down,
// which is the answer this loop is waiting for. A poll that could not be RUN is
// not that, and reading it as "the guest finished" cuts the drain from thirty
// seconds to none — a hard stop of a guest that was still syncing, on the first
// tick, silently. The deadline bounds the wait either way, so an unaskable host
// costs exactly what a wedged guest costs and no data.
func TestStopDoesNotCutTheDrainShortOnAPollItCouldNotRun(t *testing.T) {
	commands := stopCommands()
	fake := newFakeCommands()
	fake.reply(commands.socket, true)
	fake.deny(commands.poll)
	noLeftoverClones(fake, commands)

	if err := newTestManager(fake).Stop(
		context.Background(), nil, testUUID, StopRequest{},
	); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	polls := countTrace(fake, "? "+commands.poll)
	wantPolls := int(gracefulShutdownTimeout / gracefulPollInterval)
	if polls != wantPolls {
		t.Errorf("polled %d times, want the full %d: an unanswerable poll is not a halted guest",
			polls, wantPolls)
	}
}

// A refused power-off is a declined courtesy, not a failed stop.
func TestStopContinuesWhenTheGuestRefusesThePowerOff(t *testing.T) {
	commands := stopCommands()
	fake := newFakeCommands()
	fake.reply(commands.socket, true)
	fake.reply(commands.powerOff, false)
	noLeftoverClones(fake, commands)

	if err := newTestManager(fake).Stop(
		context.Background(), nil, testUUID, StopRequest{},
	); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertTrace(t, fake,
		"? "+commands.socket,
		commands.powerOff,
		commands.stop,
		"? "+commands.rootClone,
		"? "+commands.dataClone,
	)
}

func TestStopForcedNeverTouchesTheGuest(t *testing.T) {
	commands := stopCommands()
	fake := newFakeCommands()
	noLeftoverClones(fake, commands)

	if err := newTestManager(fake).Stop(
		context.Background(), nil, testUUID, StopRequest{Forced: true},
	); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertTrace(t, fake,
		commands.stop,
		"? "+commands.rootClone,
		"? "+commands.dataClone,
	)
}

func TestStopBoundsTheDrainWithARuntimeDropIn(t *testing.T) {
	commands := stopCommands()
	fake := newFakeCommands()
	noLeftoverClones(fake, commands)
	request := StopRequest{Forced: true, TimeoutSeconds: 5}

	if err := newTestManager(fake).Stop(context.Background(), nil, testUUID, request); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertTrace(t, fake,
		commands.dropInMkdir,
		commands.dropInWrite,
		commands.daemonReload,
		commands.stop,
		"- "+commands.dropInRemove,
		"- "+commands.dropInRmdir,
		"- "+commands.daemonReload,
		"? "+commands.rootClone,
		"? "+commands.dataClone,
	)
}

// Skipping ExecStopPost leaves the host answering proxy-NDP for a /128 it no
// longer owns, which the next keep-address migration then collides with. The
// bounded drain must therefore always be a stop.
func TestStopNeverKillsTheUnit(t *testing.T) {
	commands := stopCommands()
	fake := newFakeCommands()
	noLeftoverClones(fake, commands)
	request := StopRequest{TimeoutSeconds: 5}

	if err := newTestManager(fake).Stop(context.Background(), nil, testUUID, request); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for _, line := range fake.trace {
		if strings.Contains(line, "systemctl kill") || strings.Contains(line, "SIGKILL") {
			t.Fatalf("stop reached for a kill: %s", line)
		}
	}
}

func TestStopRemovesTheDropInEvenWhenTheStopFails(t *testing.T) {
	commands := stopCommands()
	fake := newFakeCommands()
	fake.reply(commands.stop, false)
	request := StopRequest{Forced: true, TimeoutSeconds: 5}

	err := newTestManager(fake).Stop(context.Background(), nil, testUUID, request)

	if err == nil {
		t.Fatal("Stop succeeded, want the failed stop reported")
	}
	// No clone convergence: the guest may still be up, and removing a clone out
	// from under a running guest is the one thing this must never do.
	assertTrace(t, fake,
		commands.dropInMkdir,
		commands.dropInWrite,
		commands.daemonReload,
		commands.stop,
		"- "+commands.dropInRemove,
		"- "+commands.dropInRmdir,
		"- "+commands.daemonReload,
	)
}

func TestStopConvergesALeftoverClone(t *testing.T) {
	commands := stopCommands()
	fake := newFakeCommands()
	fake.reply(commands.rootClone, true)
	fake.reply(commands.dataClone, true)

	if err := newTestManager(fake).Stop(
		context.Background(), nil, testUUID, StopRequest{Forced: true},
	); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertTrace(t, fake,
		commands.stop,
		"? "+commands.rootClone,
		"- sudo dmsetup remove atlas-vm-"+testUUID+"-clone",
		"? "+commands.dataClone,
		"- sudo dmsetup remove atlas-vm-"+testUUID+"-data-clone",
	)
}

func countTrace(fake *fakeCommands, line string) int {
	count := 0
	for _, recorded := range fake.trace {
		if recorded == line {
			count++
		}
	}
	return count
}
