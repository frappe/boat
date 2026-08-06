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
		exists("test -e /sys/block/nbd0/pid").
		exists("test -e /sys/block/nbd1/pid").
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
		"? test -e /sys/block/nbd0/pid",
		"sudo dmsetup message "+vmCloneRoot+" 0 enable_hydration",
		"sudo dmsetup status "+vmCloneRoot,
		// data clone: present, source alive, enable + read
		"? sudo dmsetup info "+vmCloneData,
		"- sudo dmsetup table "+vmCloneData,
		"- readlink /sys/dev/block/43:1",
		"? test -e /sys/block/nbd1/pid",
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
	// no /sys/block/nbd0/pid attribute → the kernel removed it on disconnect → dead

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
		probe("test -e /sys/block/nbd0/pid", run.Unknown)

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
		exists("test -e /sys/block/nbd2/pid").
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

// The regression a live 2-host migration exposed: a fully-hydrated (256/256) clone
// whose source is CONNECTED (the kernel's /sys/block/nbdN/pid attribute is present)
// but has NO live process at that pid — the ordinary state of nbd-client's default
// netlink mode, where the configuring process exits the instant the kernel owns the
// socket. It must read 100% and healthy. The old check probed `test -d /proc/<pid>`,
// found no process, reported the source dead and returned 0%/unhealthy — so Atlas
// looped rebuilding a clone that was already done. This exercises the REAL parse
// against the dm-clone table + status strings (the api handler's fake pollHydration
// always answered 100/healthy, which is why no test caught it).
func TestPollHydrationFullyHydratedNetlinkSourceReports100(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo dmsetup info "+vmCloneRoot).
		output("sudo dmsetup table "+vmCloneRoot, "0 20971520 clone 253:5 253:4 43:0 32768").
		output("readlink /sys/dev/block/43:0", "../../nbd0").
		exists("test -e /sys/block/nbd0/pid"). // attribute present ⟺ connected; no /proc process
		output("sudo dmsetup status "+vmCloneRoot, "0 20971520 clone 128/128 32768 256/256 0 rw")

	result, err := PollHydration(context.Background(), fake, testUUID, PollHydrationParams{})
	if err != nil {
		t.Fatalf("PollHydration: %v", err)
	}
	if result.HydrationPercent != 100 || !result.SourceHealthy {
		t.Errorf("result = %+v, want 100%% healthy", result)
	}
	// Liveness must never depend on a live process at the pid — that is the netlink
	// false-negative this fixes.
	assertNotIssued(t, fake, "/proc/")
}
