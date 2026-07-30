package park

import (
	"context"
	"errors"
	"testing"
)

// A VM that was never provisioned, or one a terminate has already taken the
// sidecar from, has no /128 to trap for. Touching the host for it would install
// a trap pointing at an address this host does not hold.
func TestParkWithNoAddressTouchesNothing(t *testing.T) {
	fake := newFakeCommands().withScaffold()

	if err := newTestParker(fake).park(context.Background(), testUUID, ""); err != nil {
		t.Fatalf("park: %v", err)
	}
	assertTrace(t, fake)
}

// An address that is not a canonical IPv6 is refused before the first host
// command: the sudoers rule line carries `daddr *`, so park validating here is
// what actually stops the nft injection the reviewer proved against it.
func TestParkRefusesAnAddressThatIsNotAnIPv6(t *testing.T) {
	fake := newFakeCommands().withScaffold()

	err := newTestParker(fake).park(
		context.Background(), testUUID, "fd00::1 drop; add counter inet atlas INJECTED_PWNED; #",
	)
	if err == nil {
		t.Fatal("park accepted an address carrying an nft injection")
	}
	// Nothing rendered: the refusal is before ensureDevice, so a malformed address
	// never becomes a command at all.
	assertTrace(t, fake)
}

func TestParkInstallsReachabilityThenTheTrap(t *testing.T) {
	fake := newFakeCommands().withScaffold()

	if err := newTestParker(fake).park(context.Background(), testUUID, testAddress); err != nil {
		t.Fatalf("park: %v", err)
	}
	assertTrace(t, fake,
		"? "+deviceShow,
		deviceUp,
		"? "+listTable,
		"? "+listChain,
		"- "+defaultRoute,
		neighReplace,
		routeReplace,
		"? "+listCounter,
		addCounter,
		"- "+listChain,
		addRule,
	)
}

func TestParkCreatesTheSharedDummyWhenItIsMissing(t *testing.T) {
	// The post-reboot case the boot sweep exists for: nothing has run yet, so
	// the device the parked /128 routes out of is not there.
	fake := newFakeCommands().withScaffold()
	fake.present[deviceShow] = false

	if err := newTestParker(fake).park(context.Background(), testUUID, testAddress); err != nil {
		t.Fatalf("park: %v", err)
	}
	if fake.indexOf(t, deviceAdd) > fake.indexOf(t, deviceUp) {
		t.Error("the dummy was brought up before it was created")
	}
}

// The sweep re-parks every sleeping VM at boot, and a re-park must add nothing.
// A second rule would split the packet count across two entries and the poll
// reads one of them, so the first SYN could be trapped where nobody is looking.
func TestParkingAnAlreadyParkedVirtualMachineAddsNothing(t *testing.T) {
	fake := newFakeCommands().withScaffold()
	fake.exists(listCounter)
	fake.output(listChain, "  ip6 daddr "+testAddress+" tcp flags syn / fin,syn,rst,ack"+
		" counter name wake_"+testHex+" drop\n")

	if err := newTestParker(fake).park(context.Background(), testUUID, testAddress); err != nil {
		t.Fatalf("park: %v", err)
	}
	assertNotIssued(t, fake, "add rule")
	assertNotIssued(t, fake, "add counter")
	// Reachability is still re-asserted: that is what the boot sweep is for.
	if !fake.issued(routeReplace) || !fake.issued(neighReplace) {
		t.Error("a re-park did not re-assert the route and the proxy entry")
	}
}

// A host whose VMs are all asleep starts no VM unit, so nothing else rebuilds
// the nft scaffold after a reboot — and `nft add counter` fails outright when
// the table it names does not exist.
func TestParkRebuildsTheTableAndChainBeforeAddingTheCounter(t *testing.T) {
	fake := newFakeCommands().withScaffold()
	fake.present[listTable] = false
	fake.present[listChain] = false

	if err := newTestParker(fake).park(context.Background(), testUUID, testAddress); err != nil {
		t.Fatalf("park: %v", err)
	}
	if fake.indexOf(t, addTable) > fake.indexOf(t, addChain) {
		t.Error("the chain was created before its table")
	}
	if fake.indexOf(t, addChain) > fake.indexOf(t, addCounter) {
		t.Error("the counter was added before the chain existed")
	}
}

func TestParkLeavesAnExistingScaffoldAlone(t *testing.T) {
	fake := newFakeCommands().withScaffold()

	if err := newTestParker(fake).ensureForwardChain(context.Background()); err != nil {
		t.Fatalf("ensureForwardChain: %v", err)
	}
	assertTrace(t, fake, "? "+listTable, "? "+listChain)
}

// A host mid-reconfigure with no IPv6 default route has no uplink to answer NDP
// on. The route and the rule are still worth installing, and the next re-park
// picks the uplink up.
func TestParkWithoutAnUplinkStillInstallsTheTrap(t *testing.T) {
	fake := newFakeCommands().withScaffold()
	fake.output(defaultRoute, "[]")

	if err := newTestParker(fake).park(context.Background(), testUUID, testAddress); err != nil {
		t.Fatalf("park: %v", err)
	}
	assertNotIssued(t, fake, "neigh")
	if !fake.issued(routeReplace) || !fake.issued(addRule) {
		t.Error("the trap was not installed")
	}
}

// Fail loud: a park that could not install its route has not parked the VM, and
// reporting success would leave a VM asleep with nothing able to wake it.
func TestParkReportsAFailedCommandAndStops(t *testing.T) {
	fake := newFakeCommands().withScaffold()
	fake.fails(routeReplace)

	err := newTestParker(fake).park(context.Background(), testUUID, testAddress)

	if !errors.Is(err, errCommandFailed) {
		t.Fatalf("park = %v, want the failed command reported", err)
	}
	assertNotIssued(t, fake, "add rule")
}

func TestEnsureDeviceBringsAnExistingDummyUpWithoutRecreatingIt(t *testing.T) {
	fake := newFakeCommands().withScaffold()

	if err := newTestParker(fake).ensureDevice(context.Background()); err != nil {
		t.Fatalf("ensureDevice: %v", err)
	}
	// The `up` is unconditional: a dummy that exists but is down routes nothing.
	assertTrace(t, fake, "? "+deviceShow, deviceUp)
}

// nft refuses to delete a counter a rule still references, so this order is
// load-bearing rather than cosmetic.
func TestUnparkDeletesTheRuleBeforeItsCounter(t *testing.T) {
	fake := newFakeCommands().withScaffold()
	fake.output(listHandles, "  ip6 daddr "+testAddress+" tcp flags syn / fin,syn,rst,ack"+
		" counter name wake_"+testHex+" drop # handle 42\n")

	if err := newTestParker(fake).unpark(context.Background(), testUUID, testAddress); err != nil {
		t.Fatalf("unpark: %v", err)
	}
	assertTrace(t, fake,
		"- "+listHandles,
		"- sudo nft delete rule inet atlas forward handle 42",
		"- "+deleteCounter,
		"- "+routeDelete,
	)
}

// An ordinary start unparks too. With nothing parked it must not try to delete a
// rule it never installed.
func TestUnparkOfAVirtualMachineThatWasNeverParkedDeletesNoRule(t *testing.T) {
	fake := newFakeCommands().withScaffold()

	if err := newTestParker(fake).unpark(context.Background(), testUUID, testAddress); err != nil {
		t.Fatalf("unpark: %v", err)
	}
	assertNotIssued(t, fake, "delete rule")
	// The counter and the route still go: both are idempotent removals, and a
	// VM that WAS parked is unparked by exactly this sequence.
	if !fake.issued(deleteCounter) || !fake.issued(routeDelete) {
		t.Error("unpark left the counter or the route behind")
	}
}

// The bring-up re-asserts the proxy entry and the teardown deletes it. Removing
// it here would strip the address off a VM on its way back up, which is the one
// thing an unpark runs in front of.
func TestUnparkLeavesProxyNeighbourDiscoveryAlone(t *testing.T) {
	fake := newFakeCommands().withScaffold()

	if err := newTestParker(fake).unpark(context.Background(), testUUID, testAddress); err != nil {
		t.Fatalf("unpark: %v", err)
	}
	assertNotIssued(t, fake, "neigh")
}

func TestUnparkWithNoAddressStillRemovesTheTrap(t *testing.T) {
	// A terminate that already took the sidecar still has to have its rule and
	// counter removed; only the route needs an address to name.
	fake := newFakeCommands().withScaffold()

	if err := newTestParker(fake).unpark(context.Background(), testUUID, ""); err != nil {
		t.Fatalf("unpark: %v", err)
	}
	if !fake.issued(deleteCounter) {
		t.Error("the counter was left behind")
	}
	assertNotIssued(t, fake, "route del")
}

// A non-zero exit is expected here — every removal deletes something an ordinary
// start never installed. A command that could not be RUN at all is different: the
// unpark did not happen, and a bring-up that rebuilt the real path over a
// surviving drop rule would leave the VM answering nothing.
func TestUnparkReportsACommandThatCouldNotRunAtAll(t *testing.T) {
	fake := newFakeCommands().withScaffold()
	fake.fails(listHandles)

	err := newTestParker(fake).unpark(context.Background(), testUUID, testAddress)

	if !errors.Is(err, errCommandFailed) {
		t.Fatalf("unpark = %v, want the failure reported", err)
	}
	assertNotIssued(t, fake, "delete counter")
}

// A terminate has no ExecStopPost coming — the unit is already inactive, and
// `systemctl disable --now` does not re-run one — so this is the last moment
// this host can stop answering NDP for the /128. Left behind, the upstream
// router goes on delivering that address here, and the day Atlas re-allocates it
// to a VM on another host, two hosts answer for one address.
func TestRetireTakesTheProxyNeighbourEntryThatUnparkLeaves(t *testing.T) {
	fake := newFakeCommands().withScaffold()

	if err := newTestParker(fake).retire(context.Background(), testUUID, testAddress); err != nil {
		t.Fatalf("retire: %v", err)
	}
	assertTrace(t, fake,
		"- "+listHandles,
		"- "+deleteCounter,
		"- "+routeDelete,
		"- "+defaultRoute,
		"- sudo ip -6 neigh del proxy "+testAddress+" dev eth0",
	)
}

// A VM whose sidecar an earlier attempt already took still has a rule and a
// counter on this host: both are named after the UUID rather than the address, so
// both still go. Only the two that need an address to name are skipped.
func TestRetireWithNoAddressStillRemovesTheTrap(t *testing.T) {
	fake := newFakeCommands().withScaffold()

	if err := newTestParker(fake).retire(context.Background(), testUUID, ""); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if !fake.issued(deleteCounter) {
		t.Error("the counter was left behind")
	}
	assertNotIssued(t, fake, "neigh")
	assertNotIssued(t, fake, "route del")
}

// The teardown reads its address off disk rather than being handed one, and it
// splices it into `route del` and `neigh del proxy` — two grants that end in a
// wildcard. An address that could never have been parked is treated as an absent
// one: nothing was installed for it, so nothing is removed, and the rule and
// counter that ARE named after the UUID still go. A terminate is not failed over
// a garbled sidecar, because that would be a VM nobody can delete.
func TestRetireWillNotRenderAnAddressThatCouldNeverHaveBeenParked(t *testing.T) {
	fake := newFakeCommands().withScaffold()

	err := newTestParker(fake).retire(
		context.Background(), testUUID, "2001:db8::1; nft flush ruleset",
	)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if !fake.issued(deleteCounter) {
		t.Error("the counter was left behind")
	}
	assertNotIssued(t, fake, "neigh")
	assertNotIssued(t, fake, "route del")
}

// A host with no IPv6 default route has no uplink it could have answered NDP on,
// so there is no entry to delete — the park that would have installed one skipped
// it for the same reason.
func TestRetireOnAHostWithNoUplinkDeletesNoNeighbourEntry(t *testing.T) {
	fake := newFakeCommands().withScaffold().output(defaultRoute, "")

	if err := newTestParker(fake).retire(context.Background(), testUUID, testAddress); err != nil {
		t.Fatalf("retire: %v", err)
	}
	assertNotIssued(t, fake, "neigh")
}

// A retire that could not run its removals reports it, so the terminate above it
// stops while the sidecar naming the address is still on disk.
func TestRetireReportsACommandThatCouldNotRunAtAll(t *testing.T) {
	fake := newFakeCommands().withScaffold()
	fake.fails(listHandles)

	err := newTestParker(fake).retire(context.Background(), testUUID, testAddress)

	if !errors.Is(err, errCommandFailed) {
		t.Fatalf("retire = %v, want the failure reported", err)
	}
	assertNotIssued(t, fake, "neigh")
}
