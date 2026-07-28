package park

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/frappe/boat/internal/run"
)

func recordWakes(woken *[]string) func(context.Context, string) error {
	return func(_ context.Context, uuid string) error {
		*woken = append(*woken, uuid)
		return nil
	}
}

func TestTheFirstTrappedSYNWakesTheVirtualMachine(t *testing.T) {
	fake := newFakeCommands()
	fake.output(listCounters, counterListing(wakeCounter{name: "wake_" + testHex, packets: 1}))
	fake.exists(markerOf(testUUID))
	var woken []string

	newTestTrap(fake, recordWakes(&woken)).tick(context.Background())

	if len(woken) != 1 || woken[0] != testUUID {
		t.Errorf("woken = %v, want [%s]", woken, testUUID)
	}
}

// A counter that has not moved is a VM nobody has asked for. Parking installs
// the counter at zero, so "no SYN yet" is the state every sleeping VM is in.
func TestACounterThatHasNotMovedWakesNothing(t *testing.T) {
	fake := newFakeCommands()
	fake.output(listCounters, counterListing(wakeCounter{name: "wake_" + testHex, packets: 0}))
	fake.exists(markerOf(testUUID))
	var woken []string
	trap := newTestTrap(fake, recordWakes(&woken))

	trap.tick(context.Background())
	trap.tick(context.Background())

	if len(woken) != 0 {
		t.Errorf("woken = %v, want nothing", woken)
	}
	// Not even the marker is read: a zero counter is answered by the poll alone.
	assertNotIssued(t, fake, "test -f")
}

// The marker is the authority, not the counter. The bring-up removes the counter
// while it rebuilds the real path, so a count outlives the sleep by a moment —
// by which time an operator's start or an earlier tick has already woken the VM.
func TestAStaleCounterOnAnAlreadyWokenVirtualMachineWakesNothing(t *testing.T) {
	fake := newFakeCommands()
	fake.output(listCounters, counterListing(wakeCounter{name: "wake_" + testHex, packets: 5}))
	var woken []string

	newTestTrap(fake, recordWakes(&woken)).tick(context.Background())

	if len(woken) != 0 {
		t.Errorf("woken = %v, want nothing: the VM is not asleep any more", woken)
	}
}

func TestOneFailedWakeDoesNotSkipTheOthers(t *testing.T) {
	fake := newFakeCommands()
	fake.output(listCounters, counterListing(
		wakeCounter{name: "wake_" + testHex, packets: 1},
		wakeCounter{name: "wake_" + otherHex, packets: 1},
	))
	fake.exists(markerOf(testUUID)).exists(markerOf(otherUUID))
	var woken []string
	wake := func(_ context.Context, uuid string) error {
		if uuid == otherUUID {
			return errors.New("systemctl failed")
		}
		woken = append(woken, uuid)
		return nil
	}

	newTestTrap(fake, wake).tick(context.Background())

	if len(woken) != 1 || woken[0] != testUUID {
		t.Errorf("woken = %v, want [%s]", woken, testUUID)
	}
}

// The wake is the caller's verb. This package must not reach for the marker or
// the unit itself: the caller's wake carries the rule this package cannot see —
// a VM an operator stopped is not resurrected by a stranger's SYN — and it is
// where the operation record lives.
func TestTheTrapNeverWakesAVirtualMachineItself(t *testing.T) {
	fake := newFakeCommands()
	fake.output(listCounters, counterListing(wakeCounter{name: "wake_" + testHex, packets: 1}))
	fake.exists(markerOf(testUUID))

	newTestTrap(fake, func(context.Context, string) error { return nil }).tick(context.Background())

	for _, forbidden := range []string{"systemctl", "rm -f", "nft delete"} {
		assertNotIssued(t, fake, forbidden)
	}
}

// A tick that failed must not take the daemon down: the next one is a second
// away, the counter is still non-zero, and the client is still retransmitting.
func TestATickThatCannotReadTheCountersKeepsPolling(t *testing.T) {
	fake := newFakeCommands()
	fake.fails(listCounters)
	journal := captureJournal(t)
	trap := newTestTrap(fake, recordWakes(&[]string{}))
	clock, _ := trap.clock.(*fakeClock)
	clock.ticks = 2

	if err := trap.Run(context.Background(), time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if polls := strings.Count(strings.Join(fake.trace, "\n"), listCounters); polls != 3 {
		t.Errorf("polled %d times, want 3", polls)
	}
	if !strings.Contains(journal.String(), "could not read the wake counters") {
		t.Errorf("the failure was not reported: %s", journal.String())
	}
}

func TestRunPollsAtTheIntervalItIsGiven(t *testing.T) {
	fake := newFakeCommands()
	trap := newTestTrap(fake, recordWakes(&[]string{}))
	clock := trap.clock.(*fakeClock)
	clock.ticks = 2

	if err := trap.Run(context.Background(), 1500*time.Millisecond); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, waited := range clock.waited {
		if waited != 1500*time.Millisecond {
			t.Errorf("waited %s between ticks, want 1.5s", waited)
		}
	}
	if len(clock.waited) != 3 {
		t.Errorf("waited %d times, want 3", len(clock.waited))
	}
}

// A cancelled context is how the daemon is asked to stop, and it must not have
// to finish waiting out a poll interval first.
func TestTheRealClockReturnsAsSoonAsTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if (systemClock{}).Wait(ctx, time.Hour) {
		t.Error("the clock waited past a cancelled context")
	}
}

func TestTheBootSweepReParksEveryMarkedVirtualMachine(t *testing.T) {
	fake := newFakeCommands().withScaffold()
	fake.withSleeping(testUUID, testAddress)

	newTestTrap(fake, recordWakes(&[]string{})).sweep(context.Background())

	if !fake.issued(deviceUp) {
		t.Error("the shared dummy was not brought up")
	}
	if !fake.issued(addCounter) || !fake.issued(addRule) {
		t.Errorf("the trap was not re-armed:\n  %s", strings.Join(fake.trace, "\n  "))
	}
	if !fake.issued(neighReplace) || !fake.issued(routeReplace) {
		t.Error("parked reachability was not rebuilt")
	}
}

// The marker is the whole of the enrolment: a VM that is merely stopped is not
// parked, and parking it would answer NDP for an address nothing here holds.
func TestTheBootSweepIgnoresAVirtualMachineWithNoMarker(t *testing.T) {
	fake := newFakeCommands().withScaffold()
	fake.withDirectory(otherUUID)

	newTestTrap(fake, recordWakes(&[]string{})).sweep(context.Background())

	assertNotIssued(t, fake, "add rule")
	assertNotIssued(t, fake, "neigh replace")
}

func TestOneFailedReParkDoesNotStopTheSweep(t *testing.T) {
	fake := newFakeCommands().withScaffold()
	fake.withSleeping(testUUID, testAddress).withSleeping(otherUUID, otherAddress)
	fake.fails(routeReplace)
	journal := captureJournal(t)

	newTestTrap(fake, recordWakes(&[]string{})).sweep(context.Background())

	if !fake.issued("counter name wake_" + otherHex) {
		t.Errorf("the second VM was not re-parked:\n  %s", strings.Join(fake.trace, "\n  "))
	}
	if !strings.Contains(journal.String(), testUUID) {
		t.Errorf("the failed re-park was not reported: %s", journal.String())
	}
}

// A VM whose sidecar a terminate already took has no /128 to trap for. Parking
// it would install a rule for an address this host no longer holds.
func TestTheBootSweepSkipsAVirtualMachineWithNoSidecar(t *testing.T) {
	fake := newFakeCommands().withScaffold()
	fake.withSleeping(testUUID, testAddress)
	fake.output(environmentOf(testUUID), "")

	newTestTrap(fake, recordWakes(&[]string{})).sweep(context.Background())

	assertNotIssued(t, fake, "add rule")
	assertNotIssued(t, fake, "neigh replace")
}

// Said once, and the poll still starts: the counters of any VM that IS still
// parked are read regardless of whether the sweep could enumerate anything.
func TestAnUnreadableVirtualMachineDirectoryLeavesTheSweepEmpty(t *testing.T) {
	fake := newFakeCommands().withScaffold()
	fake.fails(listVirtualMachines)
	journal := captureJournal(t)
	trap := newTestTrap(fake, recordWakes(&[]string{}))

	if err := trap.Run(context.Background(), time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertNotIssued(t, fake, "add rule")
	if !fake.issued(listCounters) {
		t.Error("the poll did not start")
	}
	if !strings.Contains(journal.String(), "could not list the virtual machines") {
		t.Errorf("the failure was not reported: %s", journal.String())
	}
}

// The rare event is loud and the per-second one is silent. A poll that said
// something every second would put tens of thousands of lines a day in the
// journal, and the one line an operator needs would be buried in them.
func TestThePollIsSilentAndTheWakeIsNot(t *testing.T) {
	fake := newFakeCommands()
	fake.output(listCounters, counterListing(wakeCounter{name: "wake_" + testHex, packets: 0}))
	fake.exists(markerOf(testUUID))
	journal := captureJournal(t)
	trap := newTestTrap(fake, recordWakes(&[]string{}))

	trap.tick(context.Background())

	if journal.Len() != 0 {
		t.Errorf("an uneventful poll wrote to the journal: %s", journal.String())
	}

	fake.output(listCounters, counterListing(wakeCounter{name: "wake_" + testHex, packets: 1}))
	trap.tick(context.Background())

	if !strings.Contains(journal.String(), testUUID) {
		t.Errorf("the wake was not recorded: %s", journal.String())
	}
}

// The other half of the same property, one layer down: the poll's commands go
// through a runner that has no trace writer at all, so nothing it runs can reach
// the daemon's record. The sweep — rare, and a real mutation — goes through the
// runner it was given.
func TestTheTrapPollsThroughARunnerThatCannotTrace(t *testing.T) {
	var record strings.Builder
	trap := NewTrap(run.NewRunner(&record), func(context.Context, string) error { return nil })
	// A command no host has. The lookup fails, so nothing is executed — but a
	// runner writes its `+ command` line BEFORE running anything, so a traced
	// runner leaves a line behind even for a command that never started.
	probe := "boat-park-trace-probe-that-cannot-exist"

	_, _ = trap.poller.commands.Run(context.Background(), probe)

	if record.Len() != 0 {
		t.Fatalf("the poll wrote to the daemon's record: %q", record.String())
	}

	_, _ = trap.sweeper.commands.Run(context.Background(), probe)

	if !strings.Contains(record.String(), probe) {
		t.Errorf("the sweep did not reach the daemon's record: %q", record.String())
	}
}

// The address comes out of the VM's own sidecar rather than from a caller, so a
// trap can never be armed for a /128 this host stopped holding. The file's
// syntax is internal/sidecar's business; what is this package's is that the key
// read is the public /128 and not one of the four others sharing the file.
func TestTheAddressIsReadFromTheVirtualMachinesOwnSidecar(t *testing.T) {
	fake := newFakeCommands()
	fake.outputs[environmentOf(testUUID)] = environmentText(testAddress)
	parker := &parker{commands: fake, filesFor: testFiles}

	if got := parker.address(context.Background(), testUUID); got != testAddress {
		t.Errorf("address = %q, want %q", got, testAddress)
	}
}

// A VM whose sidecar is gone — a terminate that got as far as the files —
// yields no address, and parking an empty address is a no-op rather than a trap
// installed for whatever the host holds now.
func TestAMissingSidecarYieldsNoAddress(t *testing.T) {
	parker := &parker{commands: newFakeCommands(), filesFor: testFiles}

	if got := parker.address(context.Background(), testUUID); got != "" {
		t.Errorf("address = %q, want none", got)
	}
}
