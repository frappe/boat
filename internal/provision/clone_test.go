package provision

import (
	"context"
	"strings"
	"testing"
)

// The two paths whose disk provision must NOT touch: a warm clone, whose disk has
// to stay a byte-exact CoW of the golden because the frozen RAM's filesystem cache
// references exactly those blocks, and a boot-on-clone migration, whose plain LV is
// held busy under a live dm-clone and is incomplete until collapse.

const (
	warmDirectory = "/var/lib/atlas/snapshots/golden-warm"
	cloneDevice   = "/dev/mapper/atlas-vm-" + testUUID + "-clone"
	snapshotLV    = "/dev/atlas/atlas-snap-golden"
)

func warmParams() Params {
	params := testParams()
	params.SnapshotRootfsPath = snapshotLV
	params.WarmSnapshotDirectory = warmDirectory
	return params
}

// warmHost has the golden's disk snapshot and both halves of its memory pair.
func warmHost() *fakeCommands {
	return readyHost().
		exists("sudo lvs --noheadings atlas/atlas-snap-golden").
		exists("sudo test -s " + warmDirectory + "/vmstate.bin").
		exists("sudo test -s " + warmDirectory + "/mem.bin")
}

// TestAWarmCloneStagesTheGoldenPairAndNeverTouchesTheDisk.
//
// Two orderings in here are the correctness of the whole path. The disk gets a BARE
// snapshot — no lvextend, no tune2fs, no identity injection — because any offline
// mutation would corrupt the guest that resumes onto it. And the pair is
// hard-linked AFTER the recursive chown, because a chown of a hard link chowns the
// shared inode, which N clones map read-only.
func TestAWarmCloneStagesTheGoldenPairAndNeverTouchesTheDisk(t *testing.T) {
	fake := warmHost()

	result, err := Provision(context.Background(), fake, warmParams(), refuseInject(t))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if !result.WarmPairStaged {
		t.Error("a warm clone whose disk this run created staged no pair")
	}

	assertTrace(t, fake,
		"? test -f "+testImage+"/rootfs.ext4",
		"? test -d /var/lib/atlas/virtual-machines",
		"sudo ls -1 /var/lib/atlas/virtual-machines",
		"install-dir 0700 "+testDirectory,
		"install-dir 0700 "+testDirectory+"/log",
		"install-dir 0700 "+testJailRoot,
		"install-dir 0700 "+testJailRoot+"/run",
		"? sudo test -f "+testJailRoot+"/snapshot/READY",
		"sudo rm -rf "+testJailRoot+"/snapshot",
		"? sudo lvs --noheadings atlas/atlas-snap-golden",
		// The disk did not exist, so this run created it and may stage RAM onto it.
		"? sudo lvs --noheadings "+rootReference,
		"? sudo lvs --noheadings "+rootReference,
		"sudo lvcreate -s atlas/atlas-snap-golden -n atlas-vm-"+testUUID,
		"sudo lvchange -ay -K "+rootReference,
		"sudo udevadm settle",
		"? test -b "+testDevice,
		"sudo ln -f "+testImage+"/vmlinux-6.8 "+testJailRoot+"/vmlinux",
		"install-file 0644 "+testJailRoot+"/firecracker.json",
		"lsblk -ndo MAJ:MIN "+testDevice,
		"sudo rm -f "+testJailRoot+"/rootfs.ext4",
		"sudo mknod "+testJailRoot+"/rootfs.ext4 b 252 5",
		"sudo chown 255999:255999 "+testJailRoot+"/rootfs.ext4",
		"sudo chmod 0660 "+testJailRoot+"/rootfs.ext4",
		// 4d: the identity the disk was not given, staged for the metadata service.
		"install-file 0644 "+testJailRoot+"/metadata.json",
		"sudo chown -R 255999:255999 "+testDirectory+"/jail",
		// 5b: the golden pair, hard-linked in behind a marker written LAST.
		"install-dir 0700 "+testJailRoot+"/snapshot",
		"sudo chown 255999:255999 "+testJailRoot+"/snapshot",
		"? sudo test -s "+warmDirectory+"/vmstate.bin",
		"sudo ln -f "+warmDirectory+"/vmstate.bin "+testJailRoot+"/snapshot/vmstate.bin",
		"? sudo test -s "+warmDirectory+"/mem.bin",
		"sudo ln -f "+warmDirectory+"/mem.bin "+testJailRoot+"/snapshot/mem.bin",
		"sudo cp "+warmDirectory+"/host-signature.json "+testJailRoot+"/snapshot/host-signature.json",
		"sudo touch "+testJailRoot+"/snapshot/READY",
		"install-file 0644 "+testDirectory+"/network.env",
		"install-file 0755 "+testDirectory+"/jailer-launch.sh",
		"sudo systemctl enable firecracker-vm@"+testUUID+".service",
		"sudo systemctl start --no-block firecracker-vm@"+testUUID+".service",
	)
}

// TestAWarmClonesDiskIsNeverGrownOrRelabelled, stated on its own because it is the
// difference between a guest that resumes and one that finds its filesystem cache
// pointing at blocks that moved.
func TestAWarmClonesDiskIsNeverGrownOrRelabelled(t *testing.T) {
	fake := warmHost()

	if _, err := Provision(context.Background(), fake, warmParams(), refuseInject(t)); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	assertNotIssued(t, fake, "lvextend")
	assertNotIssued(t, fake, "tune2fs")
}

// TestAWarmCloneOverAnAlreadyBootedDiskStagesNothing. The disk existed and no
// marker was pending, which together mean the guest ran and the disk has since
// diverged from the golden's RAM. Restoring that RAM over it would corrupt the
// guest, so the next start cold-boots — and the result says so rather than leaving
// an operator to infer it from a missing marker.
func TestAWarmCloneOverAnAlreadyBootedDiskStagesNothing(t *testing.T) {
	fake := warmHost().exists("sudo lvs --noheadings " + rootReference)

	result, err := Provision(context.Background(), fake, warmParams(), refuseInject(t))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if result.WarmPairStaged {
		t.Error("RAM was staged over a disk that had already been booted")
	}
	assertNotIssued(t, fake, "sudo touch "+testJailRoot+"/snapshot/READY")
	assertNotIssued(t, fake, "sudo ln -f "+warmDirectory)
}

// TestAnUnconsumedMarkerLetsAReRunStageAgain. A marker still present proves the
// previously staged pair was never consumed — the guest never ran — so the disk has
// NOT diverged and an idempotent re-run may stage it again.
func TestAnUnconsumedMarkerLetsAReRunStageAgain(t *testing.T) {
	fake := warmHost().
		exists("sudo lvs --noheadings " + rootReference).
		exists("sudo test -f " + testJailRoot + "/snapshot/READY")

	result, err := Provision(context.Background(), fake, warmParams(), refuseInject(t))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if !result.WarmPairStaged {
		t.Error("a pair that was never consumed was not re-staged")
	}
	assertIssued(t, fake, "sudo touch "+testJailRoot+"/snapshot/READY")
}

// TestAnEmptyGoldenFileIsRefused. `test -s`, not `-f`: a zero-length vmstate or
// mem file is a half-written golden, and a guest restored from one never comes
// back. The message names the file and the fix.
func TestAnEmptyGoldenFileIsRefused(t *testing.T) {
	fake := readyHost().
		exists("sudo lvs --noheadings atlas/atlas-snap-golden").
		exists("sudo test -s " + warmDirectory + "/vmstate.bin")

	_, err := Provision(context.Background(), fake, warmParams(), refuseInject(t))

	if err == nil || !strings.Contains(err.Error(), "warm snapshot file missing or empty: "+warmDirectory+"/mem.bin") {
		t.Fatalf("an empty mem.bin gave %v", err)
	}
}

// TestAWarmCloneCannotCarryADataDisk. The golden was captured without one, and the
// frozen RAM references only the root disk, so there is nothing for a second drive
// to be consistent with.
func TestAWarmCloneCannotCarryADataDisk(t *testing.T) {
	params := warmParams()
	params.DataDiskGB = 50

	_, err := Provision(context.Background(), warmHost(), params, refuseInject(t))

	if err == nil || !strings.Contains(err.Error(), "a warm clone cannot carry a data disk") {
		t.Fatalf("a warm clone with a data disk gave %v", err)
	}
}

// TestBootOnCloneExposesTheCloneDeviceAndLeavesTheLVAlone.
//
// The dest LV exists and is hydrating live, so snapshotting or growing it would
// race the read-through and corrupt the copy. The jail node is mknod'd at the CLONE
// so Firecracker reads through it; at CollapseClone the clone's table is reloaded
// onto the same dest LV, keeping the same dm major:minor and so this node and
// Firecracker's open fd both valid.
func TestBootOnCloneExposesTheCloneDeviceAndLeavesTheLVAlone(t *testing.T) {
	params := testParams()
	params.CloneRootfsDevice = cloneDevice
	fake := readyHost().
		exists("sudo dmsetup info atlas-vm-"+testUUID+"-clone").
		exists("sudo lvs --noheadings "+rootReference).
		output("lsblk -ndo MAJ:MIN "+cloneDevice, "253:1\n")

	if _, err := Provision(context.Background(), fake, params, refuseInject(t)); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	assertIssued(t, fake, "lsblk -ndo MAJ:MIN "+cloneDevice)
	assertIssued(t, fake, "sudo mknod "+testJailRoot+"/rootfs.ext4 b 253 1")
	assertNotIssued(t, fake, "lvcreate")
	assertNotIssued(t, fake, "lvextend")
	assertNotIssued(t, fake, "tune2fs")
}

// TestBootOnCloneExposesTheDataClone. The data disk is its own dm-clone, named by
// replacing the root clone's `-clone` with `-data-clone`, so /dev/vdb reads through
// until its own hydration completes.
func TestBootOnCloneExposesTheDataClone(t *testing.T) {
	dataClone := "/dev/mapper/atlas-vm-" + testUUID + "-data-clone"
	params := testParams()
	params.CloneRootfsDevice = cloneDevice
	params.DataDiskGB = 50
	fake := readyHost().
		exists("sudo dmsetup info atlas-vm-"+testUUID+"-clone").
		exists("sudo lvs --noheadings "+rootReference).
		exists("sudo lvs --noheadings "+dataReference).
		output("lsblk -ndo MAJ:MIN "+cloneDevice, "253:1\n").
		output("lsblk -ndo MAJ:MIN "+dataClone, "253:2\n")

	if _, err := Provision(context.Background(), fake, params, refuseInject(t)); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	assertIssued(t, fake, "sudo mknod "+testJailRoot+"/data.ext4 b 253 2")
	assertNotIssued(t, fake, "mkfs.ext4")
}

// TestAMissingDestLVUnderBootOnCloneNamesTheMissingPhase — for the root disk and
// for the data disk both, because the operator's fix is the same command and the
// message has to say which volume never arrived.
func TestAMissingDestLVUnderBootOnCloneNamesTheMissingPhase(t *testing.T) {
	params := testParams()
	params.CloneRootfsDevice = cloneDevice
	fake := readyHost().exists("sudo dmsetup info atlas-vm-" + testUUID + "-clone")

	_, err := Provision(context.Background(), fake, params, refuseInject(t))

	if err == nil || !strings.Contains(err.Error(), "boot-on-clone requested but dest LV atlas-vm-"+testUUID) {
		t.Fatalf("a missing dest LV gave %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "run migration-clone-target first") {
		t.Fatalf("the failure does not name the phase to run: %v", err)
	}
}

func TestAMissingDataDestLVUnderBootOnCloneIsRefused(t *testing.T) {
	params := testParams()
	params.CloneRootfsDevice = cloneDevice
	params.DataDiskGB = 50
	fake := readyHost().
		exists("sudo dmsetup info atlas-vm-" + testUUID + "-clone").
		exists("sudo lvs --noheadings " + rootReference)

	_, err := Provision(context.Background(), fake, params, refuseInject(t))

	if err == nil || !strings.Contains(err.Error(), "data dest LV atlas-data-"+testUUID) {
		t.Fatalf("a missing data dest LV gave %v", err)
	}
}

// TestACloneDeviceThatIsGoneFallsBackToTheOrdinaryPath. A collapse-forward retry —
// or any re-provision — may still carry the clone path after the disk has converged
// to the plain LV, since the clone is removed at stop. Failing on a missing clone
// would strand that retry; provisioning against the plain LV is what it means.
func TestACloneDeviceThatIsGoneFallsBackToTheOrdinaryPath(t *testing.T) {
	params := testParams()
	params.CloneRootfsDevice = cloneDevice
	fake := coldHost()

	if _, err := Provision(context.Background(), fake, params, fake.recordInject()); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	assertIssued(t, fake, "? sudo dmsetup info atlas-vm-"+testUUID+"-clone")
	assertIssued(t, fake, "sudo lvcreate -s "+baseImageReference+" -n atlas-vm-"+testUUID)
	assertIssued(t, fake, "inject "+testDevice)
	assertNotIssued(t, fake, cloneDevice)
}
