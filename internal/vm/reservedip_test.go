package vm

import (
	"context"
	"fmt"
	"testing"

	"github.com/frappe/boat/internal/netapply/reservedip"
)

// A proxy VM's network.env, carrying the two facts reserved-IP attach reads back
// off the host: the guest's private /30 and the host-side veth. The reserved IP is
// not here yet — writing it is the durable half of what attach does.
const reservedTestEnvironment = "IPV4_GUEST_CIDR=100.64.0.2/30\n" +
	"HOST_VETH=veth-abc\nVIRTUAL_MACHINE_IPV6=2001:db8::9\n"

// Attach reads the sidecar, writes the durable RESERVED_IPV4 flag, then dispatches
// the live apply with the guest and veth it read from the host — never from the
// caller — and propagates the delivery model the apply reported.
func TestReservedIPAttachWritesTheFlagThenDispatchesWithTheHostsGuestAndVeth(t *testing.T) {
	fake := newFakeCommands()
	environmentPath := testFiles(testUUID).networkEnvironment
	fake.output("sudo cat "+environmentPath, reservedTestEnvironment)
	fake.reservedDelivery = reservedip.Delivery{Anchored: true, Anchor: reservedip.Anchor{Address: "10.47.0.10", Gateway: "10.47.0.1"}}

	delivery, err := newTestManager(fake).ReservedIP(
		context.Background(), nil, testUUID, ReservedIPRequest{ReservedIPv4: "146.190.11.153"},
	)
	if err != nil {
		t.Fatalf("ReservedIP attach: %v", err)
	}
	if !delivery.Anchored || delivery.Anchor.Address != "10.47.0.10" {
		t.Errorf("delivery not propagated from the apply: %+v", delivery)
	}

	wantEnvironment := reservedTestEnvironment + "RESERVED_IPV4=146.190.11.153\n"
	assertTrace(t, fake,
		"sudo cat "+environmentPath,
		fmt.Sprintf("install -m 0644 %q %s", wantEnvironment, environmentPath),
		"attach-reserved-ip 100.64.0.2 veth-abc 146.190.11.153",
	)
}

// Detach removes the durable flag and tears the NAT down keyed on the guest — the
// reserved IP is not needed and is not read.
func TestReservedIPDetachRemovesTheFlagAndTearsDownByGuest(t *testing.T) {
	fake := newFakeCommands()
	environmentPath := testFiles(testUUID).networkEnvironment
	fake.output("sudo cat "+environmentPath, reservedTestEnvironment+"RESERVED_IPV4=146.190.11.153\n")

	if _, err := newTestManager(fake).ReservedIP(
		context.Background(), nil, testUUID, ReservedIPRequest{Detach: true},
	); err != nil {
		t.Fatalf("ReservedIP detach: %v", err)
	}

	assertTrace(t, fake,
		"sudo cat "+environmentPath,
		fmt.Sprintf("install -m 0644 %q %s", reservedTestEnvironment, environmentPath),
		"detach-reserved-ip 100.64.0.2",
	)
}

// A VM whose sidecar names no private v4 has nothing to NAT to: the verb fails
// before writing the flag or dispatching, so no half-attach is left behind.
func TestReservedIPRefusesAVMWithNoGuestAddress(t *testing.T) {
	fake := newFakeCommands()
	environmentPath := testFiles(testUUID).networkEnvironment
	fake.output("sudo cat "+environmentPath, "HOST_VETH=veth-abc\nVIRTUAL_MACHINE_IPV6=2001:db8::9\n")

	if _, err := newTestManager(fake).ReservedIP(
		context.Background(), nil, testUUID, ReservedIPRequest{ReservedIPv4: "146.190.11.153"},
	); err == nil {
		t.Fatal("attach accepted a VM with no IPV4_GUEST_CIDR")
	}
	for _, forbidden := range []string{"install -m", "attach-reserved-ip"} {
		for _, line := range fake.trace {
			if len(line) >= len(forbidden) && line[:len(forbidden)] == forbidden {
				t.Errorf("a refused attach still issued %q", line)
			}
		}
	}
}
