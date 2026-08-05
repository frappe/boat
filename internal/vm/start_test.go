package vm

import (
	"context"
	"strings"
	"testing"
)

// The four command lines a start is made of, rendered against testFiles.
func startCommands() (marker, start, resetFailed, isActive string) {
	files := testFiles(testUUID)
	return "sudo test -f " + files.memorySnapshotMarker,
		"sudo systemctl start " + files.unit,
		"sudo systemctl reset-failed " + files.unit,
		"sudo systemctl is-active " + files.unit
}

func TestStartColdBoots(t *testing.T) {
	marker, start, _, isActive := startCommands()
	fake := newFakeCommands()
	fake.reply(marker, false)

	restored, err := newTestManager(fake).Start(context.Background(), nil, testUUID)

	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if restored {
		t.Error("restored = true, want false: there was no memory snapshot to restore from")
	}
	assertTrace(t, fake, "? "+marker, start, isActive)
}

func TestStartRestoresFromAMemorySnapshot(t *testing.T) {
	marker, start, _, isActive := startCommands()
	fake := newFakeCommands()
	fake.reply(marker, true)

	restored, err := newTestManager(fake).Start(context.Background(), nil, testUUID)

	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !restored {
		t.Error("restored = false, want true: the marker was present and the start succeeded")
	}
	assertTrace(t, fake, "? "+marker, start, isActive)
}

// The failure this whole retry exists for: the restore consumed the marker and
// failed the start job, and Restart=always has already scheduled a relaunch
// that would bring the VM up behind a Failed operation.
func TestStartRetriesOnceWhenTheRestoreFailed(t *testing.T) {
	marker, start, resetFailed, isActive := startCommands()
	fake := newFakeCommands()
	fake.reply(marker, true, false)
	fake.reply(start, false, true)

	restored, err := newTestManager(fake).Start(context.Background(), nil, testUUID)

	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if restored {
		t.Error("restored = true, want false: the retry cold-boots, the snapshot is gone")
	}
	assertTrace(t, fake, "? "+marker, start, "? "+marker, resetFailed, start, isActive)
}

func TestStartDoesNotRetryWhenThereWasNoSnapshotToRestore(t *testing.T) {
	marker, start, _, _ := startCommands()
	fake := newFakeCommands()
	fake.reply(marker, false)
	fake.reply(start, false)

	_, err := newTestManager(fake).Start(context.Background(), nil, testUUID)

	if err == nil {
		t.Fatal("Start succeeded, want the start failure reported")
	}
	assertTrace(t, fake, "? "+marker, start)
}

// A start that failed with the marker still in place never attempted a restore,
// so it failed on its own merits and retrying would just fail again.
func TestStartDoesNotRetryWhenTheMarkerSurvivedTheFailure(t *testing.T) {
	marker, start, _, _ := startCommands()
	fake := newFakeCommands()
	fake.reply(marker, true, true)
	fake.reply(start, false)

	_, err := newTestManager(fake).Start(context.Background(), nil, testUUID)

	if err == nil {
		t.Fatal("Start succeeded, want the start failure reported")
	}
	assertTrace(t, fake, "? "+marker, start, "? "+marker)
}

// Exactly one retry. A second failure is a failure to boot, not a failure to
// restore, and looping on it would hide a broken VM behind a slow operation.
func TestStartRetriesAtMostOnce(t *testing.T) {
	marker, start, resetFailed, _ := startCommands()
	fake := newFakeCommands()
	fake.reply(marker, true, false)
	fake.reply(start, false)

	_, err := newTestManager(fake).Start(context.Background(), nil, testUUID)

	if err == nil {
		t.Fatal("Start succeeded, want the second start failure reported")
	}
	assertTrace(t, fake, "? "+marker, start, "? "+marker, resetFailed, start)
}

// A start refuses to guess at the marker the whole restore is keyed on.
//
// The marker lives inside the jail, under root-owned 0700 directories. Read as a
// bool, a denial is "no snapshot" — so a VM that would have resumed from RAM in
// milliseconds cold-boots instead, the operation reports restored=false, and
// nothing anywhere says the question was never asked. Nothing is started on the
// strength of it either: the probe is the verb's first act, so the refusal leaves
// a VM down rather than one that came up wrong.
func TestStartRefusesWhenItCouldNotReadTheMemorySnapshotMarker(t *testing.T) {
	marker, start, _, _ := startCommands()
	fake := newFakeCommands()
	fake.deny(marker)

	restored, err := newTestManager(fake).Start(context.Background(), nil, testUUID)

	if err == nil {
		t.Fatal("Start succeeded over a marker it could not read")
	}
	if restored {
		t.Error("restored = true on a start that never ran")
	}
	assertNotIssued(t, fake, start)
}

// The SECOND read of the marker decides between "the restore failed, retry it
// cold" and "the boot failed, report it", and it runs against the same jail the
// start just failed in.
//
// A denial read as "the marker is gone" sends a plain boot failure down the retry
// path; read the other way round it leaves the operation Failed while
// Restart=always brings the VM up five seconds later behind the controller's
// back. Neither is a guess worth making, so the start's own failure and the
// unreadable marker are reported together and nothing is retried.
func TestStartReportsAMarkerItCouldNotReReadAlongsideTheStartFailure(t *testing.T) {
	marker, start, resetFailed, _ := startCommands()
	fake := newFakeCommands()
	fake.reply(marker, true)
	fake.reply(start, false)
	fake.denyFrom(marker, 1)

	_, err := newTestManager(fake).Start(context.Background(), nil, testUUID)

	if err == nil {
		t.Fatal("Start succeeded over a marker it could not re-read")
	}
	if !strings.Contains(err.Error(), errCommandFailed.Error()) {
		t.Errorf("got %q, want the start's own failure kept beside the unreadable marker", err)
	}
	assertNotIssued(t, fake, resetFailed)
}

func TestStartFailsWhenTheUnitDoesNotSettle(t *testing.T) {
	marker, start, _, isActive := startCommands()
	fake := newFakeCommands()
	fake.reply(marker, true)
	fake.reply(isActive, false)

	restored, err := newTestManager(fake).Start(context.Background(), nil, testUUID)

	if err == nil {
		t.Fatal("Start succeeded, want the failed boot surfaced by is-active")
	}
	if restored {
		t.Error("restored = true on a failed start, want false")
	}
	assertTrace(t, fake, "? "+marker, start, isActive)
}
