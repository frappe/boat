package park

import (
	"context"
	"testing"
)

func TestCountersMapEveryWakeCounterToItsVirtualMachine(t *testing.T) {
	fake := newFakeCommands()
	fake.output(listCounters, counterListing(
		wakeCounter{name: "wake_" + testHex, packets: 3},
		wakeCounter{name: "wake_" + otherHex, packets: 0},
	))

	counters, err := newTestParker(fake).counters(context.Background())

	if err != nil {
		t.Fatalf("counters: %v", err)
	}
	if counters[testUUID] != 3 || counters[otherUUID] != 0 || len(counters) != 2 {
		t.Errorf("counters = %v", counters)
	}
	// One command for the whole host: the cost of the reflex must not grow with
	// the number of sleeping VMs, and it is unchecked because a host with no
	// atlas table is a host with no wake counters rather than a failure.
	assertTrace(t, fake, "- "+listCounters)
}

// Another feature's named counter sharing the table must not yield a UUID: the
// trap would try to wake a VM this host has never heard of.
func TestCountersIgnoreNamesThatAreNotOurs(t *testing.T) {
	fake := newFakeCommands()
	fake.output(listCounters, counterListing(
		wakeCounter{name: "bytes_total", packets: 9},
		wakeCounter{name: "wake_" + testHex, packets: 1},
	))

	counters, err := newTestParker(fake).counters(context.Background())

	if err != nil {
		t.Fatalf("counters: %v", err)
	}
	if len(counters) != 1 || counters[testUUID] != 1 {
		t.Errorf("counters = %v, want only %s", counters, testUUID)
	}
}

// The first VM to sleep creates the table. Until then nft exits non-zero saying
// there is none, and the daemon has to keep polling through that.
func TestCountersOnAHostWithNoAtlasTableAreEmpty(t *testing.T) {
	counters, err := parseCounters("")
	if err != nil || len(counters) != 0 {
		t.Errorf("parseCounters(\"\") = %v, %v; want empty and no error", counters, err)
	}
}

// Unlike the Python original, malformed output is reported. A tick logs it and
// polls again a second later, so saying so costs nothing — and a broken nft that
// returned silence would look exactly like a host with no sleeping VMs.
func TestMalformedCounterOutputIsReported(t *testing.T) {
	counters, err := parseCounters("{not json")
	if err == nil {
		t.Error("parseCounters accepted malformed output")
	}
	if len(counters) != 0 {
		t.Errorf("parseCounters = %v, want empty", counters)
	}
}

// nft is free to put element kinds in that array this does not know, and a
// future nft must not be able to stop a host waking its VMs.
func TestCountersSkipElementsThatAreNotCounters(t *testing.T) {
	output := `{"nftables":[{"metainfo":{"version":"1.0.9"}},"junk",{"table":{"name":"atlas"}},` +
		`{"counter":{"name":"wake_` + testHex + `","packets":7}}]}`

	counters, err := parseCounters(output)

	if err != nil {
		t.Fatalf("parseCounters: %v", err)
	}
	if len(counters) != 1 || counters[testUUID] != 7 {
		t.Errorf("counters = %v", counters)
	}
}

func TestCountersReportARunnerThatCouldNotRunNft(t *testing.T) {
	fake := newFakeCommands()
	fake.fails(listCounters)

	if _, err := newTestParker(fake).counters(context.Background()); err == nil {
		t.Error("a poll that could not run nft reported no error")
	}
}
