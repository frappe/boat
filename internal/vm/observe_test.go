package vm

import (
	"context"
	"testing"

	"github.com/frappe/boat/internal/fcattach"
	"github.com/frappe/boat/internal/model"
)

func observeCommands() (show, sleeping, snapshot string) {
	files := testFiles(testUUID)
	return "systemctl show " + files.unit +
			" --property=LoadState --property=ActiveState --property=SubState",
		"sudo test -f " + files.sleepingMarker,
		"sudo test -f " + files.memorySnapshotMarker
}

// livenessProbe is the trace line the Firecracker cross-check leaves. It appears
// only for a VM whose unit claims to be up, which is the whole of when the answer
// can change the status.
const livenessProbe = "liveness " + testUUID

// aHostSaying answers the three questions every observation asks: what systemd
// says about the unit, and whether each of the two markers is on disk.
func aHostSaying(unitState string, sleeping bool, snapshot bool) *fakeCommands {
	show, sleepingMarker, snapshotMarker := observeCommands()
	fake := newFakeCommands()
	fake.output(show, unitState)
	fake.reply(sleepingMarker, sleeping)
	fake.reply(snapshotMarker, snapshot)
	return fake
}

// observing runs the observation and fails on a host that could not be read at
// all. The two tests that want that failure call Observe themselves.
func observing(t *testing.T, fake *fakeCommands) model.VirtualMachine {
	t.Helper()
	observed, err := newTestManager(fake).Observe(context.Background(), nil, testUUID)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return observed
}

func assertStatus(t *testing.T, observed model.VirtualMachine, want model.VirtualMachineStatus) {
	t.Helper()
	if observed.ObservedStatus != want {
		t.Errorf("status = %q, want %q", observed.ObservedStatus, want)
	}
}

func TestObserveAsksTheHostInAFixedOrder(t *testing.T) {
	show, sleeping, snapshot := observeCommands()
	fake := aHostSaying("ActiveState=active\nSubState=running\n", false, false)

	observed := observing(t, fake)

	assertTrace(t, fake, show, "? "+sleeping, "? "+snapshot, livenessProbe)
	if observed.UUID != testUUID {
		t.Errorf("UUID = %q, want %q", observed.UUID, testUUID)
	}
	if observed.ObservedAt.IsZero() {
		t.Error("ObservedAt is zero: an observation is only worth as much as its timestamp")
	}
}

func TestObserveReportsRunningForAnActiveUnitWithALiveFirecracker(t *testing.T) {
	fake := aHostSaying("ActiveState=active\nSubState=running\n", false, false)

	observed := observing(t, fake)

	assertStatus(t, observed, model.StatusRunning)
	if observed.UnitActiveState != "active" || observed.UnitSubState != "running" {
		t.Errorf("unit state = %q/%q, want active/running", observed.UnitActiveState, observed.UnitSubState)
	}
	// The pid comes from the same authority the liveness claim does, and it is
	// the diagnostic that lets an operator strace the right process.
	if observed.FirecrackerPID != testFirecrackerPID {
		t.Errorf("pid = %d, want %d", observed.FirecrackerPID, testFirecrackerPID)
	}
}

// The defect this cross-check exists for. Pause goes through the Firecracker API
// and leaves the unit active, so systemd reports a frozen guest and a running one
// identically — and pause frees CPU rather than memory, so there is no other
// symptom to notice it by. Without asking the Firecracker, Paused has no producer
// at all and a paused VM is invisible to Atlas, to the desk and to the mirror.
func TestObserveReportsPausedWhenTheGuestIsFrozen(t *testing.T) {
	fake := aHostSaying("ActiveState=active\nSubState=running\n", false, false)
	fake.liveness.process.State = fcattach.StatePaused

	observed := observing(t, fake)

	assertStatus(t, observed, model.StatusPaused)
	if observed.UnitActiveState != "active" {
		t.Errorf("unit state = %q, want the unit still reported active", observed.UnitActiveState)
	}
}

func TestObserveReportsStoppedForAnInactiveUnit(t *testing.T) {
	observed := observing(t, aHostSaying("ActiveState=inactive\nSubState=dead\n", false, false))

	assertStatus(t, observed, model.StatusStopped)
}

// A failed unit reports Failed, not Stopped.
//
// The two are different facts and Atlas cannot act on the difference if the
// observer erases it: Stopped is a VM that was asked to stop and did, while
// Failed is one whose guest died or whose start limit tripped. Reading a failed
// unit as Stopped is the same conflation as setting status from whether a
// command succeeded, which is the thing this whole split exists to end.
func TestObserveReportsFailedForAFailedUnit(t *testing.T) {
	observed := observing(t, aHostSaying("ActiveState=failed\nSubState=failed\n", false, false))

	assertStatus(t, observed, model.StatusFailed)
}

// The marker outranks the unit state, because a sleeping VM's unit is inactive
// by construction and only the marker separates parked from stopped.
func TestObserveReportsSleepingWhenTheMarkerIsPresent(t *testing.T) {
	observed := observing(t, aHostSaying("ActiveState=inactive\nSubState=dead\n", true, true))

	assertStatus(t, observed, model.StatusSleeping)
	if !observed.Sleeping || !observed.HasMemorySnapshot {
		t.Errorf("markers = sleeping:%v snapshot:%v, want both true",
			observed.Sleeping, observed.HasMemorySnapshot)
	}
}

// A unit mid-transition is not evidence for either answer, and Boat does not
// guess on the host's behalf.
func TestObserveReportsUnknownForAUnitInTransition(t *testing.T) {
	observed := observing(t, aHostSaying("ActiveState=activating\nSubState=start-post\n", false, true))

	assertStatus(t, observed, model.StatusUnknown)
}

// The case the cross-check was added for, and the one it must not overreach on.
// systemd says the unit is active; nothing answers the socket. Those two host
// facts disagree, so Boat has not observed a state — Unknown, which means "I
// could not see". It is emphatically NOT Stopped and NOT Failed: nothing asked
// this VM to stop and its unit did not fail, and a socket that does not answer is
// not proof a VM is gone.
func TestObserveReportsUnknownWhenTheUnitIsActiveAndNothingAnswers(t *testing.T) {
	show, sleeping, snapshot := observeCommands()
	fake := aHostSaying("ActiveState=active\nSubState=running\n", false, false)
	fake.liveness = fakeLiveness{}

	observed := observing(t, fake)

	assertStatus(t, observed, model.StatusUnknown)
	if observed.FirecrackerPID != 0 {
		t.Errorf("pid = %d, want none for a VM nothing answered for", observed.FirecrackerPID)
	}
	// The unit's own words are still reported verbatim. They are what makes the
	// disagreement visible to an operator rather than only to this function.
	if observed.UnitActiveState != "active" {
		t.Errorf("unit state = %q, want the unit's claim recorded as it was made", observed.UnitActiveState)
	}
	assertTrace(t, fake, show, "? "+sleeping, "? "+snapshot, livenessProbe)
}

// A probe that could not be made is a failure to observe, not an observation.
// It comes back Unknown with the error, exactly as an unreadable systemctl does,
// so a denied sudo or a missing curl never reads as a VM that went away.
func TestObserveReportsUnknownAndTheErrorWhenTheProbeFails(t *testing.T) {
	fake := aHostSaying("ActiveState=active\nSubState=running\n", false, false)
	fake.liveness = fakeLiveness{err: errCommandFailed}

	observed, err := newTestManager(fake).Observe(context.Background(), nil, testUUID)

	if err == nil {
		t.Fatal("Observe succeeded, want the failed probe reported")
	}
	assertStatus(t, observed, model.StatusUnknown)
}

// A guest state this build does not know is Unknown rather than a guess. The
// third state Firecracker has today is "Not started" — a VMM up with no guest in
// it, which is a VM mid-launch — and a state a later Firecracker invents lands
// here too. Unknown is what nothing acts on; Running would be a fact nobody saw.
func TestObserveReportsUnknownForAGuestStateItDoesNotKnow(t *testing.T) {
	for name, state := range map[string]string{
		"not started yet": fcattach.StateNotStarted,
		"a later version": "Hibernated",
		"unreadable body": "",
	} {
		t.Run(name, func(t *testing.T) {
			fake := aHostSaying("ActiveState=active\nSubState=running\n", false, false)
			fake.liveness.process.State = state

			assertStatus(t, observing(t, fake), model.StatusUnknown)
		})
	}
}

// A VM whose unit is not claiming a live Firecracker is not asked about one. The
// answer could not change the status — the marker and the unit state have already
// settled it — and a scan of a host with dozens of stopped VMs should not open a
// socket per VM to be told what it already knows.
func TestObserveDoesNotProbeAVirtualMachineThatIsNotClaimedToBeUp(t *testing.T) {
	for name, unitState := range map[string]string{
		"stopped":     "ActiveState=inactive\nSubState=dead\n",
		"failed":      "ActiveState=failed\nSubState=failed\n",
		"activating":  "ActiveState=activating\nSubState=start-pre\n",
		"unreadable":  "",
		"no such key": "LoadState=not-found\n",
	} {
		t.Run(name, func(t *testing.T) {
			show, sleeping, snapshot := observeCommands()
			fake := aHostSaying(unitState, false, false)

			observing(t, fake)

			assertTrace(t, fake, show, "? "+sleeping, "? "+snapshot)
		})
	}
}

// A sleeping VM is not probed either: the marker outranks everything, and a
// parked VM's Firecracker is gone by construction — that is what freed the RAM.
func TestObserveDoesNotProbeASleepingVirtualMachine(t *testing.T) {
	show, sleeping, snapshot := observeCommands()
	fake := aHostSaying("ActiveState=inactive\nSubState=dead\n", true, true)

	observing(t, fake)

	assertTrace(t, fake, show, "? "+sleeping, "? "+snapshot)
}

// An unreadable host is Unknown plus the error — never a claim about the VM.
func TestObserveReportsUnknownWhenTheHostCannotBeRead(t *testing.T) {
	show, _, _ := observeCommands()
	fake := newFakeCommands()
	fake.reply(show, false)

	observed, err := newTestManager(fake).Observe(context.Background(), nil, testUUID)

	if err == nil {
		t.Fatal("Observe succeeded, want the read failure reported")
	}
	assertStatus(t, observed, model.StatusUnknown)
	if observed.UUID != testUUID || observed.ObservedAt.IsZero() {
		t.Error("an unreadable host still owes a record of when we tried")
	}
	assertTrace(t, fake, show)
}

// The cascade this package's Probe seam exists for, spelled end to end.
//
// The sleeping marker lives in a 0700 root-owned tree and is read through sudo,
// so a host missing one sudoers line answers the probe with a denial rather than
// with "no". Asked as a bool, that denial IS "no marker" — and no marker over an
// inactive unit is Stopped. Stopped is a status the reconciler acts on: it plans
// a start, the unit's own ConditionPathExists=! sees the marker that is really
// there and skips the unit with exit 0, the pass's `systemctl is-active` then
// fails, and it backs off and does the same thing again forever with no path
// back to a running VM.
//
// Unknown is the status nothing acts on, and "I could not look" is what it means.
func TestObserveWillNotCallASleepingVirtualMachineStoppedBecauseItCouldNotReadTheMarker(t *testing.T) {
	_, sleeping, _ := observeCommands()
	fake := aHostSaying("ActiveState=inactive\nSubState=dead\n", true, true)
	fake.deny(sleeping)

	observed, err := newTestManager(fake).Observe(context.Background(), nil, testUUID)

	if err == nil {
		t.Fatal("Observe succeeded over a marker it could not read")
	}
	if observed.ObservedStatus == model.StatusStopped {
		t.Fatal("a marker that could not be read was reported as a VM that is stopped, " +
			"which is the status the reconciler starts")
	}
	assertStatus(t, observed, model.StatusUnknown)
}

// The memory snapshot marker gets the same treatment, and for a reported reason
// rather than a planned one: HasMemorySnapshot rides out in the export, and a
// denied read collapsed to false tells Atlas a VM will cold-boot while its
// snapshot is sitting on disk.
func TestObserveWillNotReportNoMemorySnapshotBecauseItCouldNotReadTheMarker(t *testing.T) {
	_, _, snapshot := observeCommands()
	fake := aHostSaying("ActiveState=inactive\nSubState=dead\n", true, true)
	fake.deny(snapshot)

	observed, err := newTestManager(fake).Observe(context.Background(), nil, testUUID)

	if err == nil {
		t.Fatal("Observe succeeded over a marker it could not read")
	}
	if observed.HasMemorySnapshot {
		t.Error("a marker that could not be read was reported as one that is there")
	}
	assertStatus(t, observed, model.StatusUnknown)
}

// A unit systemd holds no unit file for is not a stopped VM.
//
// It answers inactive/dead, word for word what a VM an operator stopped answers,
// so without LoadState a host that lost its firecracker-vm@.service template
// reports every VM on it Stopped and the reconciler starts working through them —
// one `systemctl start` per VM per interval against a unit that does not exist.
// internal/units and internal/adopt have both asked for LoadState and dropped
// `not-found` all along; this was the one reader that did not, and it is the one
// that drives start and stop.
func TestObserveReportsUnknownForAUnitSystemdHasNeverHeardOf(t *testing.T) {
	observed := observing(t, aHostSaying(
		"LoadState=not-found\nActiveState=inactive\nSubState=dead\n", false, false,
	))

	if observed.ObservedStatus == model.StatusStopped {
		t.Fatal("a unit this host does not have was reported as a VM that is stopped")
	}
	assertStatus(t, observed, model.StatusUnknown)
}

// A unit the host HAS and cannot run is still a fact about the VM, so `masked`,
// `error` and `bad-setting` are reported as whatever the unit's ActiveState says.
// Only `not-found` means this host does not have the unit at all.
func TestObserveReportsALoadedUnitWhateverElseSystemdThinksOfIt(t *testing.T) {
	for _, loadState := range []string{"loaded", "masked", "error", "bad-setting"} {
		t.Run(loadState, func(t *testing.T) {
			observed := observing(t, aHostSaying(
				"LoadState="+loadState+"\nActiveState=inactive\nSubState=dead\n", false, false,
			))

			assertStatus(t, observed, model.StatusStopped)
		})
	}
}
