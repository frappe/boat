package adopt

import (
	"slices"
	"strings"
	"testing"
)

// Every scenario here starts from a healthy host and takes exactly one thing
// away, because that is how each of these states arises on a real host: a
// terminate that crashed part-way, a stop whose ExecStopPost never ran, an
// lvremove that landed while the tree was still there.

// withoutLine drops the enumerated lines naming fragment — the host having lost
// that artifact.
func withoutLine(lines []string, fragment string) []string {
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if !strings.Contains(line, fragment) {
			kept = append(kept, line)
		}
	}
	return kept
}

// The crash window terminate-vm.py leaves: the unit is down and the tree is
// gone, but the disks it pointed at are still in the volume group. Adopting this
// as a VM is how a controller boots something whose disk it already released.
func TestAHalfTerminatedVirtualMachineIsQuarantined(t *testing.T) {
	host := newFakeHost().withRunning(firstUUID)
	host.directories = nil
	host.units = []string{
		"firecracker-vm@" + firstUUID + ".service loaded inactive dead Firecracker VM",
	}

	result, err := host.scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertAdopted(t, result)
	assertEvidence(t, quarantineOf(t, result, firstUUID),
		"no VM directory /var/lib/atlas/virtual-machines/"+firstUUID,
		"systemd holds firecracker-vm@"+firstUUID+".service (inactive/dead)",
		"logical volume atlas-vm-"+firstUUID+" exists",
	)
	// The tree it lost was the only record of which namespace and address it
	// owned, so its leftover networking cannot be attributed back to it and is
	// reported on its own. Two records, two artifact sets, no guess joining them.
	quarantineOf(t, result, networkingOf[firstUUID].namespace)
	quarantineOf(t, result, networkingOf[firstUUID].address)
}

// A quarantined UUID is not merely kept out of the result: nothing downstream is
// ever asked about it, because every such question would be answered as if the
// artifact set were a VM.
func TestAQuarantinedVirtualMachineIsNeverObserved(t *testing.T) {
	host := newFakeHost().withRunning(firstUUID).withRunning(secondUUID)
	host.directories = withoutLine(host.directories, firstUUID)

	result, err := host.scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertAdopted(t, result, secondUUID)
	quarantineOf(t, result, firstUUID)
	if slices.Contains(host.observer.observed, firstUUID) {
		t.Errorf("observed %v, want the quarantined UUID never asked about", host.observer.observed)
	}
}

// A disk nothing on the host claims. It is the residue of a terminate that
// removed the tree and died before the lvremove, and it is not a VM: there is no
// jail, no config, and no record of what it was.
func TestALogicalVolumeWithNoUnitOrDirectoryIsQuarantined(t *testing.T) {
	host := newFakeHost()
	host.volumes = append(host.volumes,
		"  atlas-vm-"+firstUUID+",10737418240,pool0,",
		"  atlas-data-"+firstUUID+",21474836480,pool0,",
	)

	result, err := host.scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertAdopted(t, result)
	record := quarantineOf(t, result, firstUUID)
	assertEvidence(t, record, "no VM directory",
		"logical volume atlas-vm-"+firstUUID+" exists",
		"logical volume atlas-data-"+firstUUID+" exists")
	if strings.Contains(strings.Join(record.Evidence, "\n"), "systemd holds") {
		t.Errorf("evidence claims a unit this host does not have:\n%v", record.Evidence)
	}
}

func TestAUnitWithNoVirtualMachineDirectoryIsQuarantined(t *testing.T) {
	host := newFakeHost()
	host.units = []string{
		"firecracker-vm@" + firstUUID + ".service loaded active running Firecracker VM",
	}

	result, err := host.scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertAdopted(t, result)
	record := quarantineOf(t, result, firstUUID)
	assertEvidence(t, record, "no VM directory", "systemd holds firecracker-vm@"+firstUUID)
}

// The tree survived but the jail inside it did not. Only the jail carries the
// kernel, the config and the rootfs node, so there is nothing here to boot.
func TestAHalfRemovedJailIsQuarantined(t *testing.T) {
	host := newFakeHost().withStopped(firstUUID)
	host.present["sudo test -d "+jailRootOf(firstUUID)] = false

	result, err := host.scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertAdopted(t, result)
	record := quarantineOf(t, result, firstUUID)
	assertEvidence(t, record, "no jail tree "+jailRootOf(firstUUID),
		"directory /var/lib/atlas/virtual-machines/"+firstUUID+" is present")
}

// The disk is gone and the tree is not. This is the one the whole package is
// for: a rootfs node in the jail still points at a volume the pool has released.
func TestAVirtualMachineWhoseRootDiskIsGoneIsQuarantined(t *testing.T) {
	host := newFakeHost().withStopped(firstUUID)
	host.volumes = withoutLine(host.volumes, "atlas-vm-"+firstUUID)

	result, err := host.scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertAdopted(t, result)
	record := quarantineOf(t, result, firstUUID)
	assertEvidence(t, record, "root disk atlas-vm-"+firstUUID+" is absent")
}

// Without the sidecar nothing on the host says which namespace, tap, veth and
// address this UUID owns — and the teardown hook cannot run either, since it
// reads the same file.
func TestAVirtualMachineWithNoReadableNetworkEnvironmentIsQuarantined(t *testing.T) {
	// Absent and unreadable are the same answer here: terminate removes the
	// sidecar before the unit's ExecStopPost runs, so a read that comes back with
	// nothing is a host mid-teardown either way.
	for name, breakIt := range map[string]func(*fakeHost){
		"absent":     func(host *fakeHost) { delete(host.outputs, "sudo cat "+environmentPathOf(firstUUID)) },
		"unreadable": func(host *fakeHost) { host.failing["sudo cat "+environmentPathOf(firstUUID)] = true },
	} {
		t.Run(name, func(t *testing.T) {
			host := newFakeHost().withStopped(firstUUID)
			breakIt(host)

			result, err := host.scan(t)

			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			assertAdopted(t, result)
			assertEvidence(t, quarantineOf(t, result, firstUUID),
				"no readable "+environmentPathOf(firstUUID))
		})
	}
}

// An active unit whose host-side veth is gone has no path off the namespace: the
// guest is unreachable and the unit is claiming otherwise.
func TestAnActiveUnitWithNoHostVethIsQuarantined(t *testing.T) {
	host := newFakeHost().withRunning(firstUUID)
	host.links = withoutLine(host.links, networkingOf[firstUUID].hostVeth)

	result, err := host.scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertAdopted(t, result)
	assertEvidence(t, quarantineOf(t, result, firstUUID),
		"unit is active but its host-side veth atlas-h1111222 is absent")
}

// An active unit is a claim that a Firecracker is up, and a Firecracker cannot be
// up without the namespace its tap lives in.
func TestAnActiveUnitWithNoNetworkNamespaceIsQuarantined(t *testing.T) {
	host := newFakeHost().withRunning(firstUUID)
	host.namespaces = nil

	result, err := host.scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertAdopted(t, result)
	record := quarantineOf(t, result, firstUUID)
	assertEvidence(t, record, "unit is active but its network namespace atlas-111122223333 is absent")
}

func TestANamespaceWithNoTapIsQuarantined(t *testing.T) {
	host := newFakeHost().withRunning(firstUUID)
	network := networkingOf[firstUUID]
	host.present["sudo ip netns exec "+network.namespace+" ip link show "+network.tap] = false

	result, err := host.scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertAdopted(t, result)
	record := quarantineOf(t, result, firstUUID)
	assertEvidence(t, record,
		"unit is active but tap atlas-111122223 is absent from namespace atlas-111122223333",
		"network namespace atlas-111122223333 is up",
	)
}

// The socket is Firecracker's own API endpoint: it is created by the process and
// goes with it, so an active unit over no socket describes a process that is not
// there.
func TestAnActiveUnitWithNoFirecrackerSocketIsQuarantined(t *testing.T) {
	host := newFakeHost().withRunning(firstUUID)
	host.present["sudo test -S "+apiSocketOf(firstUUID)] = false

	result, err := host.scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertAdopted(t, result)
	record := quarantineOf(t, result, firstUUID)
	assertEvidence(t, record, "unit is active but the Firecracker API socket "+
		apiSocketOf(firstUUID)+" is absent")
}

// Untidiness is not ambiguity. A stop that skipped its ExecStopPost leaves the
// namespace, the veth and the proxy-NDP entry standing; the VM's identity is not
// in doubt and its disk is where it should be, so it is reported as the VM it is
// and its leftovers are not orphans — its own sidecar names them.
func TestLeftoverNetworkingOnAStoppedVirtualMachineIsNotQuarantined(t *testing.T) {
	host := newFakeHost().withStopped(firstUUID)
	network := networkingOf[firstUUID]
	host.namespaces = append(host.namespaces, network.namespace)
	host.links = append(host.links, "5: "+network.hostVeth+"@if4: <BROADCAST,UP> mtu 1500")
	host.proxies = append(host.proxies, network.address+" dev eth0 proxy")

	result, err := host.scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertAdopted(t, result, firstUUID)
	assertNotQuarantined(t, result)
}

// The artifacts a terminate leaves when the tree is already gone: nothing on the
// host can say which UUID they belonged to, so they are reported under the only
// identifier the host retained.
func TestNetworkArtifactsNoVirtualMachineClaimsAreQuarantined(t *testing.T) {
	host := newFakeHost()
	host.namespaces = []string{"atlas-deadbeefdead (id: 3)"}
	host.links = append(host.links, "9: atlas-hdeadbee@if8: <BROADCAST,UP> mtu 1500")
	host.proxies = []string{"2001:db8::99 dev eth0 proxy"}

	result, err := host.scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertAdopted(t, result)
	assertEvidence(t, quarantineOf(t, result, "atlas-deadbeefdead"),
		"network namespace atlas-deadbeefdead is up and no VM on this host claims it")
	assertEvidence(t, quarantineOf(t, result, "atlas-hdeadbee"),
		"link atlas-hdeadbee is up and no VM on this host claims it")
	assertEvidence(t, quarantineOf(t, result, "2001:db8::99"),
		"the host answers proxy-NDP for 2001:db8::99 on eth0")
}

// The park dummy and the host's own interfaces are bootstrap floor: reset-server
// keeps them deliberately, so a scan that reported them would report them on
// every host on every scan.
func TestBootstrapFloorIsNotAnOrphan(t *testing.T) {
	host := newFakeHost()
	host.links = append(host.links, "3: atlas-park0: <BROADCAST,NOARP,UP> mtu 1500")

	result, err := host.scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertNotQuarantined(t, result)
}

// Snapshot and image volumes are keyed by a snapshot UUID or an image name, not
// by a VM's. Reading them as VMs would quarantine every snapshot on the host.
func TestSnapshotAndImageVolumesAreNotVirtualMachines(t *testing.T) {
	host := newFakeHost().withStopped(firstUUID)
	host.volumes = append(host.volumes,
		"  atlas-snap-"+secondUUID+",10737418240,pool0,atlas-vm-"+firstUUID,
		"  atlas-image-bench-v16,4294967296,pool0,",
	)

	result, err := host.scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertAdopted(t, result, firstUUID)
	assertNotQuarantined(t, result)
}

// A not-found instance is a name systemd is holding open for something that
// references it, not a unit file this host has.
func TestANotFoundUnitIsNotAVirtualMachine(t *testing.T) {
	host := newFakeHost()
	host.units = []string{
		"firecracker-vm@" + firstUUID + ".service not-found inactive dead firecracker-vm@",
	}

	result, err := host.scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertAdopted(t, result)
	assertNotQuarantined(t, result)
}

// Nothing in the VM tree is silently dropped: a name that is not a UUID is
// reported rather than ignored, because the host is holding something and Boat
// cannot say what.
func TestAStrayNameInTheVirtualMachineTreeIsQuarantined(t *testing.T) {
	host := newFakeHost()
	host.directories = []string{"lost+found"}

	result, err := host.scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertEvidence(t, quarantineOf(t, result, "lost+found"),
		"/var/lib/atlas/virtual-machines/lost+found is not a VM UUID")
}
