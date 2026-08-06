package migration

import (
	"context"
	"testing"
)

const (
	vmDirectory = "/var/lib/atlas/virtual-machines/3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	vmNetEnv    = vmDirectory + "/network.env"
	vmUnit      = "firecracker-vm@3f2504e0-4f89-41d3-9a0c-0305e82c3301.service"
	rootPidFile = "/var/lib/atlas/run/migrate-nbd-11165.pid"
)

func TestCleanupSourceFull(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo test -f "+rootPidFile).
		output("sudo cat "+rootPidFile, "4242\n").
		exists("sudo lvs --noheadings atlas/" + rootSnap).
		exists("sudo lvs --noheadings atlas/" + dataSnap).
		exists("sudo test -f " + vmNetEnv).
		exists("sudo lvs --noheadings atlas/" + vmDisk).
		exists("sudo lvs --noheadings atlas/" + dataDisk)

	networkDown := func(context.Context) error { fake.record("", "vm-network-down "+testUUID); return nil }
	if err := CleanupSource(context.Background(), fake, testUUID, CleanupSourceParams{NBDPID: 4242}, networkDown); err != nil {
		t.Fatalf("CleanupSource: %v", err)
	}

	assertTrace(t, fake,
		// 1. kill the root export by pid + pidfile
		"- sudo kill 4242",
		"? sudo test -f "+rootPidFile,
		"- sudo cat "+rootPidFile,
		"- sudo kill 4242",
		"- sudo rm -f "+rootPidFile,
		// data (+1), base LV (+2), image-dir tar (+3): no pidfiles this migration
		"? sudo test -f /var/lib/atlas/run/migrate-nbd-11166.pid",
		"? sudo test -f /var/lib/atlas/run/migrate-nbd-11167.pid",
		"? sudo test -f /var/lib/atlas/run/migrate-nbd-11168.pid",
		"$ sudo rm -f /var/lib/atlas/run/migrate-base-meta-*.tar",
		// 2. remove the transient -migrate snapshots
		"? sudo lvs --noheadings atlas/"+rootSnap,
		"sudo lvremove -f atlas/"+rootSnap,
		"? sudo lvs --noheadings atlas/"+dataSnap,
		"sudo lvremove -f atlas/"+dataSnap,
		// 3. tear down the stale source VM
		"- sudo systemctl disable --now "+vmUnit,
		"? sudo test -f "+vmNetEnv,
		"vm-network-down "+testUUID,
		"- sudo rm -rf "+vmDirectory,
		// 4. remove the source disk LVs
		"? sudo lvs --noheadings atlas/"+vmDisk,
		"sudo lvremove -f atlas/"+vmDisk,
		"? sudo lvs --noheadings atlas/"+dataDisk,
		"sudo lvremove -f atlas/"+dataDisk,
	)
}

// A keep-address (permanent-forward) migration leaves the source's /128 forward path
// UP: the disk/nbd/snapshot teardown runs as always, but vm-network-down is SUPPRESSED
// so the mig6 tunnel + nft forward + proxy-NDP that carry live tenant ingress survive
// until the block drains (spec/24 §2.9.4). Change-address cleanup runs the network-down
// — TestCleanupSourceFull locks that half.
func TestCleanupSourceKeepAddressLeavesForward(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo test -f "+rootPidFile).
		output("sudo cat "+rootPidFile, "4242\n").
		exists("sudo lvs --noheadings atlas/" + rootSnap).
		exists("sudo lvs --noheadings atlas/" + dataSnap).
		exists("sudo test -f " + vmNetEnv). // present, yet must NOT be consulted or torn down
		exists("sudo lvs --noheadings atlas/" + vmDisk).
		exists("sudo lvs --noheadings atlas/" + dataDisk)

	called := false
	networkDown := func(context.Context) error { called = true; return nil }
	if err := CleanupSource(context.Background(), fake, testUUID, CleanupSourceParams{NBDPID: 4242, KeepAddress: true}, networkDown); err != nil {
		t.Fatalf("CleanupSource: %v", err)
	}
	if called {
		t.Error("keep-address cleanup ran vm-network-down and tore the forward path down")
	}
	// The forward path is left entirely alone: the sidecar is neither probed nor downed.
	assertNotIssued(t, fake, vmNetEnv)
	assertNotIssued(t, fake, "vm-network-down")
	// The rest of the source teardown still runs: nbd export killed, snapshots + disks
	// removed, the jail tree swept.
	assertIssued(t, fake, "sudo kill 4242")
	assertIssued(t, fake, "sudo lvremove -f atlas/"+rootSnap)
	assertIssued(t, fake, "sudo lvremove -f atlas/"+vmDisk)
	assertIssued(t, fake, "sudo lvremove -f atlas/"+dataDisk)
	assertIssued(t, fake, "sudo rm -rf "+vmDirectory)
}

// A re-entry after most of cleanup already ran finishes the rest and skips the
// network-down when the sidecar is already gone.
func TestCleanupSourceReentryIsClean(t *testing.T) {
	fake := newFakeCommands() // nothing present: pidfiles, snaps, sidecar, disks all gone
	called := false
	networkDown := func(context.Context) error { called = true; return nil }
	if err := CleanupSource(context.Background(), fake, testUUID, CleanupSourceParams{}, networkDown); err != nil {
		t.Fatalf("CleanupSource: %v", err)
	}
	if called {
		t.Error("network-down run when the sidecar was already gone")
	}
	// No snapshot or disk is lvremoved when none is present.
	assertNotIssued(t, fake, "lvremove")
	// The unit disable and the directory sweep still run (best-effort, idempotent).
	assertIssued(t, fake, "sudo systemctl disable --now "+vmUnit)
	assertIssued(t, fake, "sudo rm -rf "+vmDirectory)
}
