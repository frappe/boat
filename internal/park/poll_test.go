package park

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The two per-minute probes, held to the commands poll-vm-traffic.py and
// probe-woken-vms.py issue and to the decisions they reach. The decisions are the
// point: a wrong "idle" STOPS a guest, and a wrong "woken" tells Atlas a sleeping
// VM is Running.

// The forward chain as nft prints it for a VM with the two per-VM accepts the
// bring-up installs. nft quotes interface names on list and prints its counters as
// `counter packets N bytes M`, so this is the real shape rather than a tidied one.
func chainListing(inbound, outbound int) string {
	return "table inet atlas {\n" +
		"\tchain forward {\n" +
		"\t\ttype filter hook forward priority filter; policy accept;\n" +
		"\t\tip daddr 169.254.169.254 drop\n" +
		"\t\tip6 daddr " + testAddress + " oifname \"atlas-h3f2504\" counter packets 12 bytes " +
		strconv.Itoa(inbound) + " accept\n" +
		"\t\tip6 saddr " + testAddress + " iifname \"atlas-h3f2504\" counter packets 9 bytes " +
		strconv.Itoa(outbound) + " accept\n" +
		"\t}\n}\n"
}

// pollingParker is a parker whose counter files land in a temp directory, so the
// poll's own scratch state is real (it is read back by the next poll) without the
// test needing /var/lib/atlas.
func pollingParker(t *testing.T, fake *fakeCommands) *parker {
	t.Helper()
	directory := t.TempDir()
	return &parker{commands: fake, filesFor: func(uuid string) virtualMachineFiles {
		files := testFiles(uuid)
		files.trafficCounter = filepath.Join(directory, uuid+".json")
		return files
	}}
}

func targets() []TrafficTarget {
	return []TrafficTarget{{UUID: testUUID, Address: testAddress}}
}

// The first poll has no baseline, so it answers ACTIVE and records the total for
// the next one. Sleeping a VM this host has never observed is the one mistake that
// stops a guest.
func TestTheFirstPollIsAlwaysActive(t *testing.T) {
	fake := newFakeCommands().exists(listChain).output(listChain, chainListing(3456, 1200))
	parker := pollingParker(t, fake)

	active, err := parker.pollTraffic(context.Background(), targets())
	if err != nil {
		t.Fatalf("pollTraffic: %v", err)
	}

	assertTrace(t, fake, "? "+listChain, listChain)
	if !active[testUUID] {
		t.Error("a VM with no baseline was reported idle")
	}
	// Both rules are summed: a chain that ended up with two accepts for one VM must
	// not report the traffic that landed on the other as absent.
	if total := recordedTotal(t, parker); total != "{\"bytes\": 4656}" {
		t.Errorf("recorded %s, want the sum of both rules", total)
	}
}

// A second poll against an unmoved counter is the whole point of the verb: idle,
// and eligible to be slept.
func TestAnUnmovedCounterIsIdle(t *testing.T) {
	fake := newFakeCommands().exists(listChain).output(listChain, chainListing(3456, 1200))
	parker := pollingParker(t, fake)

	if _, err := parker.pollTraffic(context.Background(), targets()); err != nil {
		t.Fatalf("pollTraffic: %v", err)
	}
	active, err := parker.pollTraffic(context.Background(), targets())
	if err != nil {
		t.Fatalf("pollTraffic: %v", err)
	}

	if active[testUUID] {
		t.Error("a counter that did not move was reported active")
	}
}

func TestAMovedCounterIsActive(t *testing.T) {
	fake := newFakeCommands().exists(listChain).output(listChain, chainListing(3456, 1200))
	parker := pollingParker(t, fake)

	if _, err := parker.pollTraffic(context.Background(), targets()); err != nil {
		t.Fatalf("pollTraffic: %v", err)
	}
	fake.output(listChain, chainListing(9000, 1200))
	active, err := parker.pollTraffic(context.Background(), targets())
	if err != nil {
		t.Fatalf("pollTraffic: %v", err)
	}

	if !active[testUUID] {
		t.Error("a counter that moved was reported idle")
	}
}

// A counter does not count down. A smaller total means the chain was flushed or
// the host rebooted, and reading a reset as idleness would sleep a busy VM.
func TestACounterThatWentBackwardsIsActive(t *testing.T) {
	fake := newFakeCommands().exists(listChain).output(listChain, chainListing(3456, 1200))
	parker := pollingParker(t, fake)

	if _, err := parker.pollTraffic(context.Background(), targets()); err != nil {
		t.Fatalf("pollTraffic: %v", err)
	}
	fake.output(listChain, chainListing(10, 0))
	active, err := parker.pollTraffic(context.Background(), targets())
	if err != nil {
		t.Fatalf("pollTraffic: %v", err)
	}

	if !active[testUUID] {
		t.Error("a counter reset was reported idle")
	}
}

// No forward chain at all — the host rebooted before any VM unit started — is an
// anomaly about the HOST, so every VM answers active and nothing is listed.
func TestNoForwardChainReportsEverythingActive(t *testing.T) {
	fake := newFakeCommands() // the chain is absent
	parker := pollingParker(t, fake)

	active, err := parker.pollTraffic(context.Background(), []TrafficTarget{
		{UUID: testUUID, Address: testAddress}, {UUID: otherUUID, Address: otherAddress},
	})
	if err != nil {
		t.Fatalf("pollTraffic: %v", err)
	}

	assertTrace(t, fake, "? "+listChain)
	if !active[testUUID] || !active[otherUUID] {
		t.Errorf("a host with no forward chain reported an idle VM: %v", active)
	}
}

// An empty request asks the host nothing at all.
func TestPollingNoVirtualMachinesTouchesNothing(t *testing.T) {
	fake := newFakeCommands()
	parker := pollingParker(t, fake)

	active, err := parker.pollTraffic(context.Background(), nil)
	if err != nil {
		t.Fatalf("pollTraffic: %v", err)
	}

	if len(fake.trace) != 0 {
		t.Errorf("an empty poll ran commands: %v", fake.trace)
	}
	if active == nil {
		t.Error("an empty poll answered null rather than an empty map")
	}
}

// The listing is read CHECKED after the chain was proven to be there: reading a
// failure as an empty chain would report every VM's total as zero, which is a
// counter that never moves, which is a fleet that all goes to sleep.
func TestAFailedChainReadFailsThePoll(t *testing.T) {
	fake := newFakeCommands().exists(listChain).fails(listChain)
	parker := pollingParker(t, fake)

	if _, err := parker.pollTraffic(context.Background(), targets()); err == nil {
		t.Fatal("a chain read that failed was reported as an idle host")
	}
}

// A name that is not a UUID never becomes the path of a counter file.
func TestPollRefusesANameThatIsNotAUUID(t *testing.T) {
	fake := newFakeCommands().exists(listChain)
	parker := pollingParker(t, fake)

	_, err := parker.pollTraffic(context.Background(), []TrafficTarget{{UUID: "../../etc", Address: testAddress}})

	if err == nil {
		t.Fatal("a name that is not a UUID was accepted")
	}
	if len(fake.trace) != 0 {
		t.Errorf("it still touched the host: %v", fake.trace)
	}
}

// Woken is the marker read, inverted: present means still asleep.
func TestWokenReadsTheSleepingMarker(t *testing.T) {
	fake := newFakeCommands().exists(markerOf(testUUID))
	parker := newTestParker(fake)

	woken, err := parker.woken(context.Background(), []string{testUUID, otherUUID})
	if err != nil {
		t.Fatalf("woken: %v", err)
	}

	assertTrace(t, fake, "? "+markerOf(testUUID), "? "+markerOf(otherUUID))
	if woken[testUUID] {
		t.Error("a VM whose marker is still there was reported woken")
	}
	if !woken[otherUUID] {
		t.Error("a VM whose marker is gone was not reported woken")
	}
}

// A marker nobody could READ fails the probe. Reporting it woken would make Atlas
// flip a Sleeping row to Running for a VM that is still asleep — an observation
// laundered into status, which is the failure this daemon exists to end. The
// controller retries a minute later.
func TestWokenFailsRatherThanGuessOnAnUnreadableMarker(t *testing.T) {
	fake := newFakeCommands().fails(markerOf(testUUID))
	parker := newTestParker(fake)

	woken, err := parker.woken(context.Background(), []string{testUUID})

	if err == nil {
		t.Fatal("an unreadable marker was answered rather than reported")
	}
	if woken != nil {
		t.Errorf("it answered anyway: %v", woken)
	}
}

func TestWokenRefusesANameThatIsNotAUUID(t *testing.T) {
	fake := newFakeCommands()
	parker := newTestParker(fake)

	if _, err := parker.woken(context.Background(), []string{"../../etc"}); err == nil {
		t.Fatal("a name that is not a UUID was accepted")
	}
}

// The two `--vms-json` documents differ, and a reader that took the wrong one
// would silently poll an empty fleet.
func TestParsingTheTwoVirtualMachineDocuments(t *testing.T) {
	parsed, err := ParseTrafficTargets(`[{"name":"` + testUUID + `","ipv6_address":"` + testAddress + `"}]`)
	if err != nil {
		t.Fatalf("ParseTrafficTargets: %v", err)
	}
	if len(parsed) != 1 || parsed[0].UUID != testUUID || parsed[0].Address != testAddress {
		t.Errorf("parsed %v", parsed)
	}
	uuids, err := ParseUUIDs(`["` + testUUID + `"]`)
	if err != nil {
		t.Fatalf("ParseUUIDs: %v", err)
	}
	if len(uuids) != 1 || uuids[0] != testUUID {
		t.Errorf("parsed %v", uuids)
	}
	if _, err := ParseTrafficTargets("not json"); err == nil {
		t.Error("a document that is not JSON parsed")
	}
}

// recordedTotal is the counter file the poll just wrote, as text — the file the
// NEXT poll reads, so its bytes are what the delta is computed from.
func recordedTotal(t *testing.T, parker *parker) string {
	t.Helper()
	content, err := os.ReadFile(parker.filesFor(testUUID).trafficCounter)
	if err != nil {
		t.Fatalf("reading the counter file: %v", err)
	}
	return strings.TrimSpace(string(content))
}
