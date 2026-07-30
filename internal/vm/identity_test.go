package vm

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// The identity injection is tested through Rebuild, which is the only verb that
// performs it: what matters is not that the helpers run but that a rebuilt
// filesystem comes back out belonging to this VM.

// identityCommands is the injection half of a rebuild: the fresh filesystem
// came from the image's blocks, so everything that says which VM this is has to
// be written back into it.
func identityCommands() []string {
	networkEnvironment := "VIRTUAL_MACHINE_IPV6=2604:a880:800:c1::1\n" +
		"VIRTUAL_MACHINE_IPV4=10.201.0.2/30\n" +
		"VIRTUAL_MACHINE_IPV4_GATEWAY=10.201.0.1\n" +
		"PRIVATE_ADDRESS=\n"
	return []string{
		"sudo mktemp -d /tmp/atlas-mount-XXXXXX",
		"sudo mount " + testRootDevice + " " + testMountPoint,
		"install -d -m 0700 " + testMountPoint + "/root/.ssh",
		fmt.Sprintf("install -m 0600 %q %s/root/.ssh/authorized_keys",
			testIdentity.AuthorizedKeys+"\n", testMountPoint),
		fmt.Sprintf("install -m 0644 %q %s/etc/atlas-network.env", networkEnvironment, testMountPoint),
		fmt.Sprintf("install -m 0644 %q %s/etc/hostname", testHostname+"\n", testMountPoint),
		fmt.Sprintf("sudo sh -c tee -a %s/etc/hosts >/dev/null <<%q",
			testMountPoint, "\n127.0.1.1\t"+testHostname+"\n"),
		"? sudo test -f " + testMountPoint + "/etc/ssh/ssh_host_ed25519_key",
		fmt.Sprintf("install -m 0444 %q %s/etc/machine-id", testMachineID+"\n", testMountPoint),
		"? sudo grep -q LABEL=atlas-data " + testMountPoint + "/etc/fstab",
		"sudo mkdir -p " + testMountPoint + "/data",
		fmt.Sprintf("sudo sh -c tee -a %s/etc/fstab >/dev/null <<%q",
			testMountPoint, "LABEL=atlas-data\t/data\text4\tdefaults,nofail\t0\t2\n"),
		"- sudo umount " + testMountPoint,
		"- sudo rmdir " + testMountPoint,
	}
}

// The identity is what makes the fresh filesystem this VM's: its addresses, its
// authorized key, its hostname and machine-id. Without it a rebuilt VM answers
// as the image.
func TestRebuildWritesTheVirtualMachinesIdentityIntoTheFreshFilesystem(t *testing.T) {
	fake := newFakeCommands()
	aRebuiltHost(fake)
	request := RebuildRequest{
		Image: testImage, DiskGB: 40, FirecrackerUID: testFirecrackerUID, Identity: testIdentity,
	}

	if err := newTestManager(fake).Rebuild(context.Background(), nil, testUUID, request); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	for _, expected := range identityCommands() {
		if countTrace(fake, expected) != 1 {
			t.Errorf("missing from the rebuild: %s", expected)
		}
	}
}

// Host keys are the VM's SSH identity. Replacing them on every rebuild breaks
// every client's known_hosts, which looks exactly like an attack, so they are
// preserved and rotation is a separate explicit act.
func TestRebuildKeepsTheHostKeysTheDiskCarries(t *testing.T) {
	fake := newFakeCommands()
	aRebuiltHost(fake)
	request := RebuildRequest{
		Image: testImage, DiskGB: 40, FirecrackerUID: testFirecrackerUID, Identity: testIdentity,
	}

	if err := newTestManager(fake).Rebuild(context.Background(), nil, testUUID, request); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	for _, line := range fake.trace {
		if strings.Contains(line, "ssh-keygen") {
			t.Errorf("the rebuild rotated the VM's SSH identity: %s", line)
		}
	}
}

// The one exception: a disk carrying no host key at all gets one, because a
// keyless sshd does not start. That is a self-heal, not a rotation.
func TestRebuildGeneratesHostKeysForADiskThatHasNone(t *testing.T) {
	fake := newFakeCommands()
	aRebuiltHost(fake)
	fake.reply("sudo test -f "+testMountPoint+"/etc/ssh/ssh_host_ed25519_key", false)
	request := RebuildRequest{
		Image: testImage, DiskGB: 40, FirecrackerUID: testFirecrackerUID, Identity: testIdentity,
	}

	if err := newTestManager(fake).Rebuild(context.Background(), nil, testUUID, request); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	key := testMountPoint + "/etc/ssh/ssh_host_ed25519_key"
	for _, expected := range []string{
		"install -d -m 0755 " + testMountPoint + "/etc/ssh",
		"sudo rm -f " + testMountPoint + "/etc/ssh/ssh_host_rsa_key " + testMountPoint +
			"/etc/ssh/ssh_host_rsa_key.pub",
		"sudo rm -f " + key + " " + key + ".pub",
		"sudo ssh-keygen -q -t ed25519 -f " + key + " -N  -C root@" + testHostname,
	} {
		if countTrace(fake, expected) != 1 {
			t.Errorf("missing from the self-heal: %s\ngot:\n  %s", expected, strings.Join(fake.trace, "\n  "))
		}
	}
}

// A restored rootfs may already carry the data-disk line, and a duplicate fstab
// entry is its own failure — so the line is appended only when it is absent.
func TestRebuildDoesNotDuplicateTheDataDiskMount(t *testing.T) {
	fake := newFakeCommands()
	aRebuiltHost(fake)
	fake.reply("sudo grep -q LABEL=atlas-data "+testMountPoint+"/etc/fstab", true)
	request := RebuildRequest{
		Image: testImage, DiskGB: 40, FirecrackerUID: testFirecrackerUID, Identity: testIdentity,
	}

	if err := newTestManager(fake).Rebuild(context.Background(), nil, testUUID, request); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	for _, line := range fake.trace {
		if strings.Contains(line, "/etc/fstab >/dev/null") {
			t.Errorf("appended a second data-disk line: %s", line)
		}
	}
}

// The guest files Atlas hands over are written exactly as they arrived. Boat
// has no schema for what any of them mean, which is what keeps a service
// semantic — a routing URL, a bench setting — out of the host's vocabulary.
func TestRebuildWritesTheGuestFilesItWasHandedVerbatim(t *testing.T) {
	fake := newFakeCommands()
	aRebuiltHost(fake)
	identity := testIdentity
	identity.ExtraEnvironment = []EnvironmentFile{
		{Path: "/etc/atlas-routing.env", Content: "ROUTING_BASE_URL=https://orchestrator.blr1.frappe.dev\n"},
	}
	request := RebuildRequest{
		Image: testImage, DiskGB: 40, FirecrackerUID: testFirecrackerUID, Identity: identity,
	}

	if err := newTestManager(fake).Rebuild(context.Background(), nil, testUUID, request); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	written := fmt.Sprintf("install -m 0644 %q %s/etc/atlas-routing.env",
		identity.ExtraEnvironment[0].Content, testMountPoint)
	if countTrace(fake, written) != 1 {
		t.Errorf("missing from the rebuild: %s\ngot:\n  %s", written, strings.Join(fake.trace, "\n  "))
	}
}

// A guest path is joined onto a host mount point, so one that walks out of it
// writes on the host as root. Refused rather than sanitised.
func TestRebuildRefusesAGuestFilePathThatLeavesTheFilesystem(t *testing.T) {
	for name, path := range map[string]string{
		"relative": "etc/passwd",
		"walks up": "/etc/../../../root/.ssh/authorized_keys",
	} {
		fake := newFakeCommands()
		aRebuiltHost(fake)
		identity := testIdentity
		identity.ExtraEnvironment = []EnvironmentFile{{Path: path, Content: "x"}}
		request := RebuildRequest{
			Image: testImage, DiskGB: 40, FirecrackerUID: testFirecrackerUID, Identity: identity,
		}

		if err := newTestManager(fake).Rebuild(context.Background(), nil, testUUID, request); err == nil {
			t.Errorf("%s: the rebuild accepted %q", name, path)
		}
	}
}

// The data-disk mount point comes from the rebuild request too, and it is joined
// onto the host mount point and handed to `mkdir -p`, so a path that walks out of
// the filesystem makes a root-owned directory on the host. It is refused the same
// way a guest file path is — the sibling check writeExtraEnvironment already made.
func TestRebuildRefusesADataDiskMountThatLeavesTheFilesystem(t *testing.T) {
	for name, mountAt := range map[string]string{
		"relative": "mnt/data",
		"walks up": "/mnt/../../etc/cron.d",
	} {
		fake := newFakeCommands()
		aRebuiltHost(fake)
		identity := testIdentity
		identity.DataDiskMountAt = mountAt
		request := RebuildRequest{
			Image: testImage, DiskGB: 40, FirecrackerUID: testFirecrackerUID, Identity: identity,
		}

		if err := newTestManager(fake).Rebuild(context.Background(), nil, testUUID, request); err == nil {
			t.Errorf("%s: the rebuild accepted data-disk mount %q", name, mountAt)
		}
	}
}

func TestHostnameAndMachineIdentifierAreDerivedFromTheUUID(t *testing.T) {
	if got := hostnameFor(testUUID); got != testHostname {
		t.Errorf("hostnameFor = %q, want %q", got, testHostname)
	}
	if got := machineIdentifierFor(testUUID); got != testMachineID {
		t.Errorf("machineIdentifierFor = %q, want %q", got, testMachineID)
	}
	if got := len(machineIdentifierFor(testUUID)); got != 32 {
		t.Errorf("machine-id is %d characters, want 32", got)
	}
}
