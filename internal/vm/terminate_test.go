package vm

import (
	"context"
	"strings"
	"testing"
)

type terminateCommandSet struct {
	disable      string
	retire       string
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
		retire:       "retire " + testUUID,
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
		commands.retire,
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
		commands.retire,
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
		commands.retire,
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
	assertTrace(t, fake, "- "+commands.disable, commands.retire, commands.removeTree)
}

// The defect this file exists to keep fixed. `systemctl disable --now` on an
// ALREADY-INACTIVE unit does not re-run ExecStopPost — checked on a host, not
// reasoned about — so a stopped or sleeping VM's networking is torn down by
// nothing at all unless the terminate does it, and `rm -rf` takes the sidecar
// that names the address first. What is left behind is a forward-chain rule that
// counts and DROPS every inbound SYN to a /128 Atlas is free to hand to the next
// VM on this host.
func TestTerminateWithdrawsTheParkedNetworkOfAStoppedVirtualMachine(t *testing.T) {
	commands := terminateCommands()
	fake := newFakeCommands()
	// A VM whose unit is already inactive: the disable exits non-zero, and its
	// ExecStopPost does not run.
	fake.reply(commands.disable, false)
	fake.reply(commands.rootClone, false)
	fake.reply(commands.dataClone, false)

	if err := newTestManager(fake).Terminate(context.Background(), nil, testUUID); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if fake.trace[1] != commands.retire {
		t.Fatalf("the parked network was not withdrawn: %v", fake.trace)
	}
	// BEFORE the tree, because network.env lives in it and is the only record of
	// which address to withdraw.
	if indexOfTrace(t, fake, commands.retire) > indexOfTrace(t, fake, commands.removeTree) {
		t.Error("the VM directory was removed before its address could be withdrawn")
	}
}

// A teardown that could not be done is a terminate that must not proceed. The
// tree still holds the sidecar, so the retry can still read the address; carrying
// on would destroy the only record of what was left behind on the host.
func TestTerminateStopsWhenTheParkedNetworkCannotBeWithdrawn(t *testing.T) {
	commands := terminateCommands()
	fake := newFakeCommands()
	fake.retireError = errCommandFailed

	err := newTestManager(fake).Terminate(context.Background(), nil, testUUID)

	if err == nil {
		t.Fatal("Terminate succeeded with the parked network still installed")
	}
	if !strings.Contains(err.Error(), "parked networking") {
		t.Errorf("got %q, want an error naming what was left on the host", err)
	}
	assertTrace(t, fake, "- "+commands.disable, commands.retire)
}

// indexOfTrace is where a command appears in the recorded sequence, so an
// ordering assertion fails with the sequence rather than with two numbers.
func indexOfTrace(t *testing.T, fake *fakeCommands, command string) int {
	t.Helper()
	for index, recorded := range fake.trace {
		if recorded == command {
			return index
		}
	}
	t.Fatalf("%q was never run:\n  %s", command, strings.Join(fake.trace, "\n  "))
	return -1
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
