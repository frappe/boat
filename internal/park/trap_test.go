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

// The sleep verb refuses to park a VM unless this reflex is running (see
// internal/vm's requireWakeTrap), so what Resident answers has to be the trap's
// real lifetime rather than a flag somebody remembered to set: false before Run,
// true throughout it, and false again the moment the loop returns — which is
// exactly when a sleep must stop being allowed, because a daemon on its way down
// is a daemon that will not wake anything.
func TestResidentIsTrueOnlyWhileTheTrapIsPolling(t *testing.T) {
	fake := newFakeCommands()
	fake.output(listCounters, counterListing(wakeCounter{name: "wake_" + testHex, packets: 1}))
	fake.exists(markerOf(testUUID))
	var whileWaking bool
	trap := newTestTrap(fake, func(context.Context, string) error {
		whileWaking = Resident()
		return nil
	})

	if Resident() {
		t.Fatal("Resident answered true with no trap running in this process")
	}
	if err := trap.Run(context.Background(), time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !whileWaking {
		t.Error("Resident answered false while the trap was polling")
	}
	if Resident() {
		t.Error("Resident stayed true after the loop returned")
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

// A marker the sweep cannot READ must not be read as a VM that is not asleep.
//
// The marker is in a 0700 root-owned tree and is asked for through sudo, so a
// host missing one sudoers line answers the probe with a denial rather than with
// "no". Asked as a bool, that denial silently removed the VM from the sweep's
// list — and this sweep is the ONLY thing that re-parks after a reboot, because
// a sleeping VM's unit is suppressed by the very marker that could not be read.
// The result was a VM unreachable for as long as it slept, with nothing in the
// journal. It is still skipped, because parking a VM this sweep cannot confirm
// is asleep would black-hole a running one, but it is skipped OUT LOUD.
func TestTheBootSweepSaysSoWhenAMarkerCannotBeRead(t *testing.T) {
	fake := newFakeCommands().withScaffold()
	fake.withSleeping(testUUID, testAddress)
	fake.fails(markerOf(testUUID))
	journal := captureJournal(t)

	newTestTrap(fake, recordWakes(&[]string{})).sweep(context.Background())

	assertNotIssued(t, fake, "add rule")
	if !strings.Contains(journal.String(), testUUID) {
		t.Errorf("a marker that could not be read was skipped in silence: %s", journal.String())
	}
}

// The same third answer on the poll's side, and here it goes the other way.
//
// A counter that moved says a SYN has already arrived for this VM. Declining to
// act on it because the marker could not be read leaves that VM asleep with
// nothing on the host left to notice it — the failure OK's own doc calls "a wake
// trap declines to wake anything". Asking is safe: the wake is a REQUEST for a
// pass, and the pass re-reads the host, the desired power and the boot fence
// before it starts anything, so a wrong ask costs a no-op pass.
func TestACounterWhoseMarkerCannotBeReadStillAsksForAWake(t *testing.T) {
	fake := newFakeCommands()
	fake.output(listCounters, counterListing(wakeCounter{name: "wake_" + testHex, packets: 1}))
	fake.fails(markerOf(testUUID))
	journal := captureJournal(t)
	var woken []string

	newTestTrap(fake, recordWakes(&woken)).tick(context.Background())

	if len(woken) != 1 || woken[0] != testUUID {
		t.Errorf("woken = %v, want [%s]: a marker nobody could read is not an awake VM", woken, testUUID)
	}
	if !strings.Contains(journal.String(), "still sleeping") {
		t.Errorf("the unreadable marker was not reported: %s", journal.String())
	}
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
	fake.present[sidecarProbe(testUUID)] = true
	fake.outputs[environmentOf(testUUID)] = environmentText(testAddress)
	parker := &parker{commands: fake, filesFor: testFiles}

	got, found, err := parker.address(context.Background(), testUUID)
	if err != nil || !found || got != testAddress {
		t.Errorf("address = %q found=%v err=%v, want %q", got, found, err, testAddress)
	}
}

// A VM whose sidecar is gone — a terminate that got as far as the files —
// yields no address, and parking an empty address is a no-op rather than a trap
// installed for whatever the host holds now.
func TestAMissingSidecarYieldsNoAddress(t *testing.T) {
	parker := &parker{commands: newFakeCommands(), filesFor: testFiles}

	got, found, err := parker.address(context.Background(), testUUID)
	if err != nil || found || got != "" {
		t.Errorf("address = %q found=%v err=%v, want a proven absence", got, found, err)
	}
}

// An UNREADABLE sidecar is not a missing one, and reading it as one was an
// outage: the address came back empty, parking an empty address is a no-op, and
// Sleep reported success with the guest stopped and nothing trapping its /128.
func TestAnUnreadableSidecarIsAnErrorAndNotAnAbsentAddress(t *testing.T) {
	fake := newFakeCommands()
	fake.failing[sidecarProbe(testUUID)] = true
	parker := &parker{commands: fake, filesFor: testFiles}

	if _, _, err := parker.address(context.Background(), testUUID); err == nil {
		t.Error("a sidecar that could not be read was reported as a VM with no address")
	}
}

// The boot sweep walks a list it materialized at the top, so a VM woken while
// it works must not be re-parked. Re-parking a running VM routes its /128 into
// the black-hole dummy and drops every inbound SYN, and nothing ever undoes it:
// the poll only acts on VMs whose marker is present, and this one's is gone.
func TestTheBootSweepDoesNotReParkAVirtualMachineWokenWhileItRan(t *testing.T) {
	fake := newFakeCommands().withScaffold()
	fake.withSleeping(testUUID, testAddress)
	trap := newTestTrap(fake, recordWakes(&[]string{}))
	// The turn is where a wake would have been excluded, so it is also where the
	// marker is re-read. Clearing it here is the wake landing between the listing
	// and this VM's turn.
	trap.SerializeWith(func(ctx context.Context, uuid string, fn func(context.Context) error) error {
		fake.present[markerOf(uuid)] = false
		return fn(ctx)
	})

	trap.sweep(context.Background())

	if fake.issued(addRule) || fake.issued(routeReplace) || fake.issued(neighReplace) {
		t.Errorf("the sweep re-parked a VM that had woken:\n  %s", strings.Join(fake.trace, "\n  "))
	}
}

// The turn's re-read gets the same treatment as the listing's: a marker it could
// not READ is not a VM that woke up, so the re-park is refused and REPORTED
// rather than skipped as a no-op the way a genuine wake is.
func TestTheBootSweepReportsATurnWhoseMarkerCannotBeRead(t *testing.T) {
	fake := newFakeCommands().withScaffold()
	fake.withSleeping(testUUID, testAddress)
	journal := captureJournal(t)
	trap := newTestTrap(fake, recordWakes(&[]string{}))
	// Denied between the listing and this VM's turn, which is where the sweep asks
	// again and where the answer decides whether a running VM gets black-holed.
	trap.SerializeWith(func(ctx context.Context, uuid string, fn func(context.Context) error) error {
		fake.fails(markerOf(uuid))
		return fn(ctx)
	})

	trap.sweep(context.Background())

	if fake.issued(addRule) || fake.issued(routeReplace) {
		t.Errorf("the sweep parked a VM it could not confirm was asleep:\n  %s",
			strings.Join(fake.trace, "\n  "))
	}
	if !strings.Contains(journal.String(), "could not re-park") {
		t.Errorf("the refused re-park was not reported: %s", journal.String())
	}
}

// The sweep rebuilds the nft scaffold as well as the dummy, and for one reason
// more than the dummy has: the poll reads its counters out of `table inet atlas`
// with a CHECKED command, so the table has to be host floor rather than something
// the first sleeping VM happens to create. Idempotent on a host that has it.
func TestTheBootSweepRebuildsTheNftablesScaffold(t *testing.T) {
	fake := newFakeCommands()
	fake.outputs[defaultRoute] = defaultRouteOutput

	newTestTrap(fake, recordWakes(&[]string{})).sweep(context.Background())

	for _, expected := range []string{deviceAdd, deviceUp, addTable, addChain} {
		if !fake.issued(expected) {
			t.Errorf("the sweep did not rebuild %q:\n  %s", expected, strings.Join(fake.trace, "\n  "))
		}
	}
}

// Every re-park takes that VM's own turn, so the sweep is never a second driver
// of a machine a verb or a reconcile pass is already driving.
func TestTheBootSweepTakesEachVirtualMachinesTurn(t *testing.T) {
	fake := newFakeCommands().withScaffold()
	fake.withSleeping(testUUID, testAddress)
	trap := newTestTrap(fake, recordWakes(&[]string{}))
	var turns []string
	trap.SerializeWith(func(ctx context.Context, uuid string, fn func(context.Context) error) error {
		turns = append(turns, uuid)
		return fn(ctx)
	})

	trap.sweep(context.Background())

	if len(turns) != 1 || turns[0] != testUUID {
		t.Errorf("turns taken: %v, want one for %s", turns, testUUID)
	}
}
