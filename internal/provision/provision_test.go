package provision

import (
	"context"
	"strings"
	"testing"
)

// The commands below are spelled out because they ARE the port: provision-vm.py's
// trace on a real host is the reference, and a template that drifts from it is a
// jail built differently rather than a test out of date. The order is contract
// too — the disk exists before its identity is written into it, the jail's files
// exist before the recursive chown, and the unit starts last.

const (
	baseImageReference = "atlas/atlas-image-ubuntu-24.04"
	rootReference      = "atlas/atlas-vm-" + testUUID
	dataReference      = "atlas/atlas-data-" + testUUID
)

// coldHost is readyHost plus the base image LV every from-image provision
// snapshots from.
func coldHost() *fakeCommands {
	return readyHost().exists("sudo lvs --noheadings " + baseImageReference)
}

// TestAColdProvisionRendersThePythonsSequence: the ordinary case — a new VM from a
// base image, no data disk, no clone, no warm golden.
func TestAColdProvisionRendersThePythonsSequence(t *testing.T) {
	fake := coldHost()

	result, err := Provision(context.Background(), fake, testParams(), fake.recordInject())
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if result.WarmPairStaged {
		t.Error("a cold provision staged a warm memory pair")
	}

	assertTrace(t, fake,
		// 0. the image must be on the host; the kernel is hard-linked out of the
		// same directory whatever the rootfs source is.
		"? test -f "+testImage+"/rootfs.ext4",
		// 0b. the per-VM uid collision guard, over the VMs this host already holds.
		"? test -d /var/lib/atlas/virtual-machines",
		"sudo ls -1 /var/lib/atlas/virtual-machines",
		// The VM's own tree, every directory 0700.
		"install-dir 0700 "+testDirectory,
		"install-dir 0700 "+testDirectory+"/log",
		"install-dir 0700 "+testJailRoot,
		"install-dir 0700 "+testJailRoot+"/run",
		// A leftover memory snapshot would pair stale RAM with the fresh disk.
		"? sudo test -f "+testJailRoot+"/snapshot/READY",
		"sudo rm -rf "+testJailRoot+"/snapshot",
		// 1. the per-VM disk: a CoW thin snapshot of the base image, grown, and
		// given its own ext4 UUID so host-side blkid stays honest.
		"? sudo lvs --noheadings "+baseImageReference,
		"? sudo lvs --noheadings "+rootReference,
		"sudo lvcreate -s "+baseImageReference+" -n atlas-vm-"+testUUID,
		"sudo lvchange -ay -K "+rootReference,
		"sudo udevadm settle",
		"? test -b "+testDevice,
		"- sudo lvextend -r -L 25G "+testDevice,
		"sudo tune2fs -U random -L atlas-root "+testDevice,
		// 2. this VM's identity, written through the plain LV.
		"inject "+testDevice,
		// 3 and 4. the hard-linked kernel and the jail's Firecracker config.
		"sudo ln -f "+testImage+"/vmlinux-6.8 "+testJailRoot+"/vmlinux",
		"install-file 0644 "+testJailRoot+"/firecracker.json",
		// 4b. the disk as a block-special node the jailed Firecracker opens.
		"lsblk -ndo MAJ:MIN "+testDevice,
		"sudo rm -f "+testJailRoot+"/rootfs.ext4",
		"sudo mknod "+testJailRoot+"/rootfs.ext4 b 252 5",
		"sudo chown 255999:255999 "+testJailRoot+"/rootfs.ext4",
		"sudo chmod 0660 "+testJailRoot+"/rootfs.ext4",
		// 5. the whole tree to the per-VM uid, after every file is in place.
		"sudo chown -R 255999:255999 "+testDirectory+"/jail",
		// 6 and 7. the sidecar the network hook reads and the launcher the unit execs.
		"install-file 0644 "+testDirectory+"/network.env",
		"install-file 0755 "+testDirectory+"/jailer-launch.sh",
		// 8. enable is synchronous; start does not block on the unit going active.
		"sudo systemctl enable firecracker-vm@"+testUUID+".service",
		"sudo systemctl start --no-block firecracker-vm@"+testUUID+".service",
	)
}

// TestTheGeneratedFilesLandWhereTheJailerLooksForThem. The bytes are asserted in
// render_test.go; this pins the destinations and modes, which are what make the
// jailed Firecracker able to read its own config (0644) and systemd able to exec
// the launcher (0755).
func TestTheGeneratedFilesLandWhereTheJailerLooksForThem(t *testing.T) {
	fake := coldHost()

	if _, err := Provision(context.Background(), fake, testParams(), fake.recordInject()); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	assertFile(t, fake, testJailRoot+"/firecracker.json", "0644", wantFirecrackerConfiguration)
	assertFile(t, fake, testDirectory+"/jailer-launch.sh", "0755", wantJailerLaunch)
	if _, staged := fake.installedFile[testJailRoot+"/metadata.json"]; staged {
		t.Error("an ordinary VM was given an MMDS payload")
	}
}

// TestACloneSnapshotsTheSnapshotLV. The clone path swaps the origin and nothing
// else: the identity injected afterwards is derived from THIS VM's UUID, so a clone
// never shares host keys or machine-id with the VM it was cloned from.
func TestACloneSnapshotsTheSnapshotLV(t *testing.T) {
	params := testParams()
	params.SnapshotRootfsPath = "/dev/atlas/atlas-snap-golden"
	fake := readyHost().exists("sudo lvs --noheadings atlas/atlas-snap-golden")

	if _, err := Provision(context.Background(), fake, params, fake.recordInject()); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	assertIssued(t, fake, "sudo lvcreate -s atlas/atlas-snap-golden -n atlas-vm-"+testUUID)
	assertNotIssued(t, fake, baseImageReference)
	assertIssued(t, fake, "inject "+testDevice)
}

// TestAMissingSnapshotIsRefusedByName, and a missing base image too: both name the
// LV and the fix, because the operator's next action differs (re-take the snapshot
// against re-sync the image).
func TestAMissingSnapshotIsRefusedByName(t *testing.T) {
	params := testParams()
	params.SnapshotRootfsPath = "/dev/atlas/atlas-snap-golden"
	fake := readyHost()

	_, err := Provision(context.Background(), fake, params, fake.recordInject())

	if err == nil || !strings.Contains(err.Error(), "snapshot LV not found: atlas-snap-golden") {
		t.Fatalf("a missing snapshot gave %v", err)
	}
}

func TestAMissingBaseImageLVSaysToSync(t *testing.T) {
	fake := readyHost()

	_, err := Provision(context.Background(), fake, testParams(), fake.recordInject())

	if err == nil || !strings.Contains(err.Error(), "run Sync to Server first") {
		t.Fatalf("a missing base image LV gave %v", err)
	}
}

// TestAMissingImageDirectoryIsRefusedBeforeAnythingIsLaidDown. Image sync is
// multi-minute and deliberately not auto-triggered from provision, so the message
// tells the operator to click Sync to Server — and nothing has been created by the
// time they read it.
func TestAMissingImageDirectoryIsRefusedBeforeAnythingIsLaidDown(t *testing.T) {
	fake := newFakeCommands()

	_, err := Provision(context.Background(), fake, testParams(), fake.recordInject())

	if err == nil || !strings.Contains(err.Error(), "not present on server") {
		t.Fatalf("a missing image gave %v", err)
	}
	assertTrace(t, fake, "? test -f "+testImage+"/rootfs.ext4")
}

// TestANameThatIsNotAUUIDNeverBecomesAPath. The name is spliced into an LV
// reference and into every path under /var/lib/atlas/virtual-machines, so a `..` in
// it walks out of the tree and a space in it adds arguments to the command it lands
// in. Checked before it is rendered anywhere, and nothing runs.
func TestANameThatIsNotAUUIDNeverBecomesAPath(t *testing.T) {
	params := testParams()
	params.VirtualMachineName = "../../etc"
	fake := coldHost()

	_, err := Provision(context.Background(), fake, params, fake.recordInject())

	if err == nil || !strings.Contains(err.Error(), "is not a VM UUID") {
		t.Fatalf("a non-UUID name gave %v", err)
	}
	assertTrace(t, fake)
}

// TestAnEmptyCgroupSetIsRefused. It encodes the VM's real memory and CPU limits,
// and an empty set does not mean "no limits requested" — it means an un-bounded VM
// on a host sized for bounded ones. The Python makes the flag required; this says
// why, which is the more useful failure.
func TestAnEmptyCgroupSetIsRefused(t *testing.T) {
	params := testParams()
	params.CgroupArguments = nil
	fake := coldHost()

	_, err := Provision(context.Background(), fake, params, fake.recordInject())

	if err == nil || !strings.Contains(err.Error(), "would un-bound the VM") {
		t.Fatalf("an empty cgroup set gave %v", err)
	}
	assertTrace(t, fake)
}

// TestAUIDCollisionWithAnotherVMIsRefused. The uid is derived from the UUID and a
// mod collision is possible; two VMs sharing one uid share the DAC identity that is
// the whole of the inter-jail isolation, so each could open the other's device
// nodes. Loud is the only safe answer.
func TestAUIDCollisionWithAnotherVMIsRefused(t *testing.T) {
	const other = "11111111-2222-4333-8444-555555555555"
	otherNode := "/var/lib/atlas/virtual-machines/" + other + "/jail/firecracker/" + other + "/root/rootfs.ext4"
	fake := coldHost().
		output("sudo ls -1 /var/lib/atlas/virtual-machines", testUUID+"\n"+other+"\n").
		exists("sudo test -e "+otherNode).
		output("sudo stat -c %u "+otherNode, "255999\n")

	_, err := Provision(context.Background(), fake, testParams(), fake.recordInject())

	if err == nil || !strings.Contains(err.Error(), "uid 255999 already owned by "+otherNode) {
		t.Fatalf("a uid collision gave %v", err)
	}
}

// TestOurOwnJailNodeIsNotACollision — that is what makes a re-provision of the same
// VM (the idempotent re-run this whole verb is built to survive) pass the guard.
func TestOurOwnJailNodeIsNotACollision(t *testing.T) {
	fake := coldHost().
		exists("sudo test -e "+testJailRoot+"/rootfs.ext4").
		output("sudo stat -c %u "+testJailRoot+"/rootfs.ext4", "255999\n")

	if _, err := Provision(context.Background(), fake, testParams(), fake.recordInject()); err != nil {
		t.Fatalf("re-provisioning our own VM was refused: %v", err)
	}
	assertNotIssued(t, fake, "sudo stat -c %u")
}

// TestAVMWithNoJailNodeYetIsSkipped. The Python's glob yields only paths that
// exist, so a VM directory with no jail in it contributes nothing — and a `stat` of
// the missing node would fail the whole verb rather than skipping it.
func TestAVMWithNoJailNodeYetIsSkipped(t *testing.T) {
	const other = "11111111-2222-4333-8444-555555555555"
	fake := coldHost().output("sudo ls -1 /var/lib/atlas/virtual-machines", testUUID+"\n"+other+"\n")

	if _, err := Provision(context.Background(), fake, testParams(), fake.recordInject()); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	assertNotIssued(t, fake, "sudo stat -c %u")
}

// TestAFreshDataDiskIsFormattedOnlyOnce. mkfs on a later run would wipe the
// tenant's data, so a disk that already exists is grown and checked instead — which
// is what makes a re-provision reuse the data disk rather than reset it.
func TestAFreshDataDiskIsFormattedOnlyOnce(t *testing.T) {
	params := testParams()
	params.DataDiskGB = 50
	fake := coldHost()

	if _, err := Provision(context.Background(), fake, params, fake.recordInject()); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// The ONE line in this port that is not byte-identical to the Python: lvm.py
	// passes the pool as the bare `pool0` and internal/thinpool as the qualified
	// `atlas/pool0`. lvcreate accepts both — the VG operand disambiguates either way
	// — and the qualified form is what internal/vmdisk and internal/bootstrap have
	// been creating volumes with on live hosts. It is called out here so a
	// differential run against the Python can recognise the difference instead of
	// investigating it.
	assertIssued(t, fake, "sudo lvcreate --type thin --thinpool atlas/pool0 -V 50G -n atlas-data-"+testUUID+" atlas")
	assertIssued(t, fake, "sudo mkfs.ext4 -q -L atlas-data -F "+testDataDevice)
	// 4c: the data disk is the guest's /dev/vdb, exposed the same way the root is.
	assertIssued(t, fake, "sudo mknod "+testJailRoot+"/data.ext4 b 252 6")
}

func TestAnExistingDataDiskIsGrownNotReformatted(t *testing.T) {
	params := testParams()
	params.DataDiskGB = 50
	fake := coldHost().exists("sudo lvs --noheadings " + dataReference)

	if _, err := Provision(context.Background(), fake, params, fake.recordInject()); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	assertNotIssued(t, fake, "mkfs.ext4")
	assertIssued(t, fake, "- sudo lvextend -r -L 50G "+testDataDevice)
	assertIssued(t, fake, "- sudo e2fsck -fy "+testDataDevice)
}

// TestAnUnformattedDataDiskGrowsTheBlockDeviceOnly. There is no filesystem to -r,
// so the plain lvextend is the whole of the grow — and running resize2fs against a
// disk that is not ext4 is a conversation with a stranger's bytes.
func TestAnUnformattedDataDiskGrowsTheBlockDeviceOnly(t *testing.T) {
	params := testParams()
	params.DataDiskGB = 50
	params.DataDiskFormat = 0
	fake := coldHost().exists("sudo lvs --noheadings " + dataReference)

	if _, err := Provision(context.Background(), fake, params, fake.recordInject()); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	assertIssued(t, fake, "- sudo lvextend -L 50G "+testDataDevice)
	assertNotIssued(t, fake, "lvextend -r -L 50G "+testDataDevice)
	assertNotIssued(t, fake, "mkfs.ext4")
}

// TestACloneDataDiskKeepsItsLabel. The guest mounts by LABEL=atlas-data — the fstab
// line the identity injection writes says exactly that — so the UUID is rerolled
// and the label is kept. No mkfs: the data is what is being preserved.
func TestACloneDataDiskKeepsItsLabel(t *testing.T) {
	params := testParams()
	params.DataDiskGB = 50
	params.DataSnapshotRootfsPath = "/dev/atlas/atlas-datasnap-golden"
	fake := coldHost().exists("sudo lvs --noheadings atlas/atlas-datasnap-golden")

	if _, err := Provision(context.Background(), fake, params, fake.recordInject()); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	assertIssued(t, fake, "sudo lvcreate -s atlas/atlas-datasnap-golden -n atlas-data-"+testUUID)
	assertIssued(t, fake, "sudo tune2fs -U random -L atlas-data "+testDataDevice)
	assertNotIssued(t, fake, "mkfs.ext4")
}
