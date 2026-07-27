package vm

import (
	"context"
	"testing"

	"github.com/frappe/boat/internal/model"
)

func observeCommands() (show, sleeping, snapshot string) {
	files := testFiles(testUUID)
	return "systemctl show " + files.unit + " --property=ActiveState --property=SubState",
		"sudo test -f " + files.sleepingMarker,
		"sudo test -f " + files.memorySnapshotMarker
}

// observedWith answers the three questions an observation asks and returns what
// Boat concluded from the answers.
func observedWith(
	t *testing.T, unitState string, sleeping bool, snapshot bool,
) (model.VirtualMachine, *fakeCommands) {
	t.Helper()
	show, sleepingMarker, snapshotMarker := observeCommands()
	fake := newFakeCommands()
	fake.output(show, unitState)
	fake.reply(sleepingMarker, sleeping)
	fake.reply(snapshotMarker, snapshot)

	observed, err := newTestManager(fake).Observe(context.Background(), nil, testUUID)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return observed, fake
}

func TestObserveAsksTheHostInAFixedOrder(t *testing.T) {
	show, sleeping, snapshot := observeCommands()
	observed, fake := observedWith(t, "ActiveState=active\nSubState=running\n", false, false)

	assertTrace(t, fake, show, "? "+sleeping, "? "+snapshot)
	if observed.UUID != testUUID {
		t.Errorf("UUID = %q, want %q", observed.UUID, testUUID)
	}
	if observed.ObservedAt.IsZero() {
		t.Error("ObservedAt is zero: an observation is only worth as much as its timestamp")
	}
}

func TestObserveReportsRunningForAnActiveUnit(t *testing.T) {
	observed, _ := observedWith(t, "ActiveState=active\nSubState=running\n", false, false)

	if observed.ObservedStatus != model.StatusRunning {
		t.Errorf("status = %q, want %q", observed.ObservedStatus, model.StatusRunning)
	}
	if observed.UnitActiveState != "active" || observed.UnitSubState != "running" {
		t.Errorf("unit state = %q/%q, want active/running", observed.UnitActiveState, observed.UnitSubState)
	}
}

func TestObserveReportsStoppedForAnInactiveUnit(t *testing.T) {
	observed, _ := observedWith(t, "ActiveState=inactive\nSubState=dead\n", false, false)

	if observed.ObservedStatus != model.StatusStopped {
		t.Errorf("status = %q, want %q", observed.ObservedStatus, model.StatusStopped)
	}
}

// A failed unit is stopped, not unknown: the host answered, and the answer was
// that nothing is running.
func TestObserveReportsStoppedForAFailedUnit(t *testing.T) {
	observed, _ := observedWith(t, "ActiveState=failed\nSubState=failed\n", false, false)

	if observed.ObservedStatus != model.StatusStopped {
		t.Errorf("status = %q, want %q", observed.ObservedStatus, model.StatusStopped)
	}
}

// The marker outranks the unit state, because a sleeping VM's unit is inactive
// by construction and only the marker separates parked from stopped.
func TestObserveReportsSleepingWhenTheMarkerIsPresent(t *testing.T) {
	observed, _ := observedWith(t, "ActiveState=inactive\nSubState=dead\n", true, true)

	if observed.ObservedStatus != model.StatusSleeping {
		t.Errorf("status = %q, want %q", observed.ObservedStatus, model.StatusSleeping)
	}
	if !observed.Sleeping || !observed.HasMemorySnapshot {
		t.Errorf("markers = sleeping:%v snapshot:%v, want both true",
			observed.Sleeping, observed.HasMemorySnapshot)
	}
}

// A unit mid-transition is not evidence for either answer, and Boat does not
// guess on the host's behalf.
func TestObserveReportsUnknownForAUnitInTransition(t *testing.T) {
	observed, _ := observedWith(t, "ActiveState=activating\nSubState=start-post\n", false, true)

	if observed.ObservedStatus != model.StatusUnknown {
		t.Errorf("status = %q, want %q", observed.ObservedStatus, model.StatusUnknown)
	}
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
	if observed.ObservedStatus != model.StatusUnknown {
		t.Errorf("status = %q, want %q", observed.ObservedStatus, model.StatusUnknown)
	}
	if observed.UUID != testUUID || observed.ObservedAt.IsZero() {
		t.Error("an unreadable host still owes a record of when we tried")
	}
	assertTrace(t, fake, show)
}
