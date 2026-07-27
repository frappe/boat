package vm

import (
	"context"
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
