package paths

import (
	"strings"
	"testing"
)

// One fixed UUID, and every expectation below spelled out as a literal. A
// refactor that quietly moves a file has to edit a literal here, which is the
// point: these paths are a contract with the hosts Atlas already provisioned.
const testUUID = "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"

func TestVirtualMachinePaths(t *testing.T) {
	virtualMachine := ForVirtualMachine(testUUID)

	tests := []struct {
		name     string
		actual   string
		expected string
	}{
		{"Directory", virtualMachine.Directory(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"},
		{"LogDirectory", virtualMachine.LogDirectory(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/log"},
		{"NetworkEnvironment", virtualMachine.NetworkEnvironment(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/network.env"},
		{"FirewallEnvironment", virtualMachine.FirewallEnvironment(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/firewall.env"},
		{"TunnelsDirectory", virtualMachine.TunnelsDirectory(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/tunnels"},
		{"TunnelEnvironment", virtualMachine.TunnelEnvironment("office"),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/tunnels/office.env"},
		{"TunnelKey", virtualMachine.TunnelKey("office"),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/tunnels/office.key"},
		{"JailChrootBase", virtualMachine.JailChrootBase(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/jail"},
		{"JailRoot", virtualMachine.JailRoot(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/jail/firecracker/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/root"},
		{"RootFilesystemNode", virtualMachine.RootFilesystemNode(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/jail/firecracker/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/root/rootfs.ext4"},
		{"DataNode", virtualMachine.DataNode(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/jail/firecracker/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/root/data.ext4"},
		{"Kernel", virtualMachine.Kernel(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/jail/firecracker/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/root/vmlinux"},
		{"FirecrackerConfig", virtualMachine.FirecrackerConfig(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/jail/firecracker/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/root/firecracker.json"},
		{"JailerLaunch", virtualMachine.JailerLaunch(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/jailer-launch.sh"},
		{"MemorySnapshotDirectory", virtualMachine.MemorySnapshotDirectory(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/jail/firecracker/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/root/snapshot"},
		{"MemorySnapshotMarker", virtualMachine.MemorySnapshotMarker(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/jail/firecracker/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/root/snapshot/READY"},
		{"MemorySnapshotVMState", virtualMachine.MemorySnapshotVMState(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/jail/firecracker/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/root/snapshot/vmstate.bin"},
		{"MemorySnapshotMemory", virtualMachine.MemorySnapshotMemory(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/jail/firecracker/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/root/snapshot/mem.bin"},
		{"MemorySnapshotSignature", virtualMachine.MemorySnapshotSignature(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/jail/firecracker/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/root/snapshot/host-signature.json"},
		{"MetadataFile", virtualMachine.MetadataFile(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/jail/firecracker/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/root/metadata.json"},
		{"APISocketDirectory", virtualMachine.APISocketDirectory(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/jail/firecracker/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/root/run"},
		{"APISocket", virtualMachine.APISocket(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/jail/firecracker/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/root/run/firecracker.socket"},
		{"APISocketName", virtualMachine.APISocketName(), "firecracker.socket"},
		{"SleepingMarker", virtualMachine.SleepingMarker(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/sleeping"},
		{"TrafficCounterFile", virtualMachine.TrafficCounterFile(),
			"/var/lib/atlas/virtual-machines/a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d/traffic-counter.json"},
		{"SystemdUnit", virtualMachine.SystemdUnit(),
			"firecracker-vm@a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d.service"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.actual != test.expected {
				t.Errorf("got %q, want %q", test.actual, test.expected)
			}
		})
	}
}

func TestSharedDirectories(t *testing.T) {
	tests := []struct {
		name     string
		actual   string
		expected string
	}{
		{"AtlasRoot", AtlasRoot, "/var/lib/atlas"},
		{"ImagesDirectory", ImagesDirectory, "/var/lib/atlas/images"},
		{"VirtualMachinesDirectory", VirtualMachinesDirectory, "/var/lib/atlas/virtual-machines"},
		{"BinDirectory", BinDirectory, "/var/lib/atlas/bin"},
		{"SnapshotsDirectory", SnapshotsDirectory, "/var/lib/atlas/snapshots"},
		{"ImageDirectory", ImageDirectory("bench-v16"), "/var/lib/atlas/images/bench-v16"},
		{"WarmSnapshotDirectory", WarmSnapshotDirectory("bench-v16-warm"),
			"/var/lib/atlas/snapshots/bench-v16-warm"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.actual != test.expected {
				t.Errorf("got %q, want %q", test.actual, test.expected)
			}
		})
	}
}

// The jailer appends firecracker/<id>/root to a chroot base that is already
// per-VM, so the UUID legitimately appears twice. Assert the count, because the
// duplication reads like a bug and someone will eventually try to "fix" it.
func TestJailRootNestsUUIDTwice(t *testing.T) {
	virtualMachine := ForVirtualMachine(testUUID)

	if occurrences := strings.Count(virtualMachine.JailRoot(), testUUID); occurrences != 2 {
		t.Errorf("jail root contains the UUID %d times, want 2: %q", occurrences, virtualMachine.JailRoot())
	}
	expected := virtualMachine.JailChrootBase() + "/firecracker/" + testUUID + "/root"
	if virtualMachine.JailRoot() != expected {
		t.Errorf("got %q, want %q", virtualMachine.JailRoot(), expected)
	}
}

// The socket is exposed as two halves because one absolute path cannot serve
// both callers: stat takes any length, connect does not. If the absolute form
// ever fits in sun_path the workaround is dead weight — until then this test is
// the reason it exists.
func TestAPISocketPairStraddlesTheSunPathLimit(t *testing.T) {
	virtualMachine := ForVirtualMachine(testUUID)

	if joined := virtualMachine.APISocketDirectory() + "/" + virtualMachine.APISocketName(); joined != virtualMachine.APISocket() {
		t.Errorf("the two halves join to %q, want %q", joined, virtualMachine.APISocket())
	}
	if length := len(virtualMachine.APISocket()); length <= SunPathMax {
		t.Errorf("absolute socket path is %d bytes, expected it to exceed the %d-byte sun_path limit", length, SunPathMax)
	}
	if length := len(virtualMachine.APISocketName()); length >= SunPathMax {
		t.Errorf("relative socket name is %d bytes, which does not fit in sun_path", length)
	}
}

// Firecracker resolves API-body paths after the chroot, so each in-jail constant
// must be exactly its absolute path with the jail root stripped. Drift here
// produces a snapshot Firecracker cannot find, at snapshot time, on a live VM.
func TestInJailPathsMatchTheirAbsoluteForms(t *testing.T) {
	virtualMachine := ForVirtualMachine(testUUID)
	jailRootPrefix := virtualMachine.JailRoot() + "/"

	tests := []struct {
		name     string
		absolute string
		inJail   string
	}{
		{"vmstate", virtualMachine.MemorySnapshotVMState(), MemorySnapshotVMStateInJail},
		{"memory", virtualMachine.MemorySnapshotMemory(), MemorySnapshotMemoryInJail},
		{"metadata", virtualMachine.MetadataFile(), MetadataInJail},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if relative := strings.TrimPrefix(test.absolute, jailRootPrefix); relative != test.inJail {
				t.Errorf("%q under the jail root is %q, want %q", test.absolute, relative, test.inJail)
			}
		})
	}
}

// A UUID is a path segment and a sudoers wildcard's contents, so the shapes
// that must be refused are the ones that would escape either.
func TestIsUUIDRefusesAnythingThatIsNotOne(t *testing.T) {
	valid := "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	if !IsUUID(valid) {
		t.Fatalf("IsUUID(%q) = false, want true", valid)
	}
	for _, name := range []string{
		"",
		"..",
		"../../etc/shadow",
		"3f2504e0-4f89-41d3-9a0c-0305e82c33",      // too short
		"3f2504e0-4f89-41d3-9a0c-0305e82c330100",  // too long
		"3f2504e0-4f89-41d3-9a0c-0305e82c33g1",    // not hex
		"3F2504E0-4F89-41D3-9A0C-0305E82C3301",    // uppercase: a second spelling of one VM
		"3f2504e0/4f89-41d3-9a0c-0305e82c3301",    // a separator
		"3f2504e0-4f89-41d3-9a0c-0305e82c3301 rm", // an extra argument
		"3f2504e0-4f89-41d3-9a0c-0305e82c3301/../x",
	} {
		if IsUUID(name) {
			t.Errorf("IsUUID(%q) = true, want false", name)
		}
	}
}
