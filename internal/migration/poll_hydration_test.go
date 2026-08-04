package migration

import (
	"context"
	"testing"

	"github.com/frappe/boat/internal/run"
)

const (
	vmCloneData = "atlas-vm-3f2504e0-4f89-41d3-9a0c-0305e82c3301-data-clone"
)

// TestPollHydrationRootAndData walks the full per-tick sequence for a VM with both
// disks hydrating, and asserts the MIN percent is reported so the phase does not
// advance until BOTH are done.
func TestPollHydrationRootAndData(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo dmsetup info "+vmCloneRoot).
		exists("sudo dmsetup info "+vmCloneData).
		output("sudo dmsetup table "+vmCloneRoot, "0 20971520 clone 253:5 253:4 43:0 32768").
		output("sudo dmsetup table "+vmCloneData, "0 41943040 clone 253:7 253:6 43:1 32768").
		output("readlink /sys/dev/block/43:0", "../../nbd0").
		output("readlink /sys/dev/block/43:1", "../../nbd1").
		output("cat /sys/block/nbd0/pid", "8123\n").
		output("cat /sys/block/nbd1/pid", "8130\n").
		exists("test -d /proc/8123").
		exists("test -d /proc/8130").
		output("sudo dmsetup status "+vmCloneRoot, "0 20971520 clone 8/1024 32768 320/640 0 rw").
		output("sudo dmsetup status "+vmCloneData, "0 41943040 clone 9/1024 32768 960/1280 0 rw")

	result, err := PollHydration(context.Background(), fake, testUUID, PollHydrationParams{})
	if err != nil {
		t.Fatalf("PollHydration: %v", err)
	}
	if result.HydrationPercent != 50 || !result.SourceHealthy {
		t.Errorf("result = %+v, want 50%% healthy", result)
	}

	assertTrace(t, fake,
		// discover the data clone
		"? sudo dmsetup info "+vmCloneData,
		// root clone: present, source alive, enable + read
		"? sudo dmsetup info "+vmCloneRoot,
		"- sudo dmsetup table "+vmCloneRoot,
		"- readlink /sys/dev/block/43:0",
		"- cat /sys/block/nbd0/pid",
		"? test -d /proc/8123",
		"sudo dmsetup message "+vmCloneRoot+" 0 enable_hydration",
		"sudo dmsetup status "+vmCloneRoot,
		// data clone: present, source alive, enable + read
		"? sudo dmsetup info "+vmCloneData,
		"- sudo dmsetup table "+vmCloneData,
		"- readlink /sys/dev/block/43:1",
		"- cat /sys/block/nbd1/pid",
		"? test -d /proc/8130",
		"sudo dmsetup message "+vmCloneData+" 0 enable_hydration",
		"sudo dmsetup status "+vmCloneData,
	)
}

// A dead source client is reported unhealthy, its percent skipped — the controller
// re-runs prepare rather than advancing.
func TestPollHydrationDeadSource(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo dmsetup info "+vmCloneRoot).
		output("sudo dmsetup table "+vmCloneRoot, "0 20971520 clone 253:5 253:4 43:0 32768").
		output("readlink /sys/dev/block/43:0", "../../nbd0")
	// no pid recorded → the client died

	result, err := PollHydration(context.Background(), fake, testUUID, PollHydrationParams{})
	if err != nil {
		t.Fatalf("PollHydration: %v", err)
	}
	if result.SourceHealthy || result.HydrationPercent != 0 {
		t.Errorf("result = %+v, want 0%% unhealthy", result)
	}
	// A dead source is never messaged enable_hydration.
	assertNotIssued(t, fake, "enable_hydration")
}

// A collapsed or missing clone reads as fully hydrated, so a re-entry after cutover
// advances cleanly.
func TestPollHydrationCollapsedReports100(t *testing.T) {
	fake := newFakeCommands() // no clone present at all
	result, err := PollHydration(context.Background(), fake, testUUID, PollHydrationParams{})
	if err != nil {
		t.Fatalf("PollHydration: %v", err)
	}
	if result.HydrationPercent != 100 || !result.SourceHealthy {
		t.Errorf("result = %+v, want 100%% healthy", result)
	}
}

// A liveness probe that could not be made (a denied sudo) surfaces as an error, not
// a rounded-to-dead reading that would trigger a destructive re-prepare.
func TestPollHydrationUnknownLivenessErrors(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo dmsetup info "+vmCloneRoot).
		output("sudo dmsetup table "+vmCloneRoot, "0 20971520 clone 253:5 253:4 43:0 32768").
		output("readlink /sys/dev/block/43:0", "../../nbd0").
		output("cat /sys/block/nbd0/pid", "8123\n").
		probe("test -d /proc/8123", run.Unknown)

	if _, err := PollHydration(context.Background(), fake, testUUID, PollHydrationParams{}); err == nil {
		t.Fatal("PollHydration reported a health it could not determine")
	}
	assertNotIssued(t, fake, "enable_hydration")
}

// The CloneDevice override polls exactly one dm device (the base-image ship), with
// no VM-disk discovery.
func TestPollHydrationCloneDeviceOverride(t *testing.T) {
	base := baseCloneName("debian12")
	fake := newFakeCommands().
		exists("sudo dmsetup info "+base).
		output("sudo dmsetup table "+base, "0 8388608 clone 253:9 253:8 43:2 32768").
		output("readlink /sys/dev/block/43:2", "../../nbd2").
		output("cat /sys/block/nbd2/pid", "9001\n").
		exists("test -d /proc/9001").
		output("sudo dmsetup status "+base, "0 8388608 clone 4/1024 32768 256/256 0 rw")

	result, err := PollHydration(context.Background(), fake, testUUID, PollHydrationParams{CloneDevice: base})
	if err != nil {
		t.Fatalf("PollHydration: %v", err)
	}
	if result.HydrationPercent != 100 {
		t.Errorf("percent = %d, want 100", result.HydrationPercent)
	}
	// No VM-disk clone was consulted.
	assertNotIssued(t, fake, vmCloneRoot)
	assertNotIssued(t, fake, vmCloneData)
	assertIssued(t, fake, "sudo dmsetup message "+base+" 0 enable_hydration")
}
