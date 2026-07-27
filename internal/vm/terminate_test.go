package vm

import (
	"context"
	"testing"
)

type terminateCommandSet struct {
	disable      string
	removeTree   string
	rootClone    string
	dataClone    string
	rootExists   string
	removeRoot   string
	dataExists   string
	removeData   string
	removeClone  string
	removeClone2 string
}

func terminateCommands() terminateCommandSet {
	files := testFiles(testUUID)
	return terminateCommandSet{
		disable:      "sudo systemctl disable --now " + files.unit,
		removeTree:   "sudo rm -rf " + files.directory,
		rootClone:    "sudo dmsetup info atlas-vm-" + testUUID + "-clone",
		dataClone:    "sudo dmsetup info atlas-vm-" + testUUID + "-data-clone",
		rootExists:   "sudo lvs --noheadings atlas/atlas-vm-" + testUUID,
		removeRoot:   "sudo lvremove -f atlas/atlas-vm-" + testUUID,
		dataExists:   "sudo lvs --noheadings atlas/atlas-data-" + testUUID,
		removeData:   "sudo lvremove -f atlas/atlas-data-" + testUUID,
		removeClone:  "sudo dmsetup remove atlas-vm-" + testUUID + "-clone",
		removeClone2: "sudo dmsetup remove atlas-vm-" + testUUID + "-data-clone",
	}
}

// The order is the test. A running guest holds its root volume open, so an
// lvremove that ran before the unit came down would fail on a volume the guest
// still has a descriptor for — and the VM directory has to go before the
// volumes too, because the block nodes inside it point at them.
func TestTerminateRemovesTheUnitBeforeTheDisks(t *testing.T) {
	commands := terminateCommands()
	fake := newFakeCommands()
	fake.reply(commands.rootClone, false)
	fake.reply(commands.dataClone, false)

	if err := newTestManager(fake).Terminate(context.Background(), nil, testUUID); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	assertTrace(t, fake,
		"- "+commands.disable,
		commands.removeTree,
		"? "+commands.rootClone,
		"? "+commands.dataClone,
		"? "+commands.rootExists,
		commands.removeRoot,
		"? "+commands.dataExists,
		commands.removeData,
	)
}

// Terminating what is already gone succeeds. A retry after a partial failure
// has to be able to finish the job, and every step it re-runs will find its
// object missing.
func TestTerminateSucceedsWhenTheVirtualMachineIsAlreadyGone(t *testing.T) {
	commands := terminateCommands()
	fake := newFakeCommands()
	fake.reply(commands.disable, false)
	fake.reply(commands.rootClone, false)
	fake.reply(commands.dataClone, false)
	fake.reply(commands.rootExists, false)
	fake.reply(commands.dataExists, false)

	if err := newTestManager(fake).Terminate(context.Background(), nil, testUUID); err != nil {
		t.Fatalf("Terminate on an absent VM: %v, want success", err)
	}
	assertTrace(t, fake,
		"- "+commands.disable,
		commands.removeTree,
		"? "+commands.rootClone,
		"? "+commands.dataClone,
		"? "+commands.rootExists,
		"? "+commands.dataExists,
	)
}

// A migrated VM's leftover dm-clone holds the plain volume busy, so the
// lvremove that follows would fail with "used by another device" if the clone
// were not converged first.
func TestTerminateConvergesALeftoverCloneBeforeRemovingTheVolume(t *testing.T) {
	commands := terminateCommands()
	fake := newFakeCommands()
	fake.reply(commands.rootClone, true)
	fake.reply(commands.dataClone, false)

	if err := newTestManager(fake).Terminate(context.Background(), nil, testUUID); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	assertTrace(t, fake,
		"- "+commands.disable,
		commands.removeTree,
		"? "+commands.rootClone,
		"- "+commands.removeClone,
		"? "+commands.dataClone,
		"? "+commands.rootExists,
		commands.removeRoot,
		"? "+commands.dataExists,
		commands.removeData,
	)
}

func TestTerminateReportsATreeItCouldNotRemove(t *testing.T) {
	commands := terminateCommands()
	fake := newFakeCommands()
	fake.reply(commands.removeTree, false)

	if err := newTestManager(fake).Terminate(context.Background(), nil, testUUID); err == nil {
		t.Fatal("Terminate succeeded, want the failed removal reported")
	}
	// It stops there: the volumes are still referenced by nodes in a tree that
	// is still present, and removing them anyway would leave the worse mess.
	assertTrace(t, fake, "- "+commands.disable, commands.removeTree)
}

// The guard exists so that a bug which passed the wrong name to a per-VM verb
// can destroy at most that VM's own disk, never the pool or a base image every
// VM on the host is a snapshot of.
func TestRemoveRefusesTheSharedVolumes(t *testing.T) {
	for _, name := range []string{"pool0", "atlas-image-ubuntu-24.04"} {
		fake := newFakeCommands()
		if err := (volume{name: name}).remove(context.Background(), fake); err == nil {
			t.Errorf("removing %s succeeded, want a refusal", name)
		}
		if len(fake.trace) != 0 {
			t.Errorf("removing %s ran %v, want nothing", name, fake.trace)
		}
	}
}
