package migration

import (
	"context"
	"errors"
	"testing"
)

func TestSourceForward(t *testing.T) {
	fake := newFakeCommands().
		exists("ip link show "+tunDevice).
		output("ip -j -6 route show default", `[{"dst":"default","dev":"eth0"}]`)

	result, err := SourceForward(context.Background(), fake, testUUID, SourceForwardParams{VirtualMachineIPv6: testVMv6})
	if err != nil {
		t.Fatalf("SourceForward: %v", err)
	}
	if !result.Forwarding {
		t.Error("SourceForward did not report forwarding")
	}

	assertTrace(t, fake,
		"? ip link show "+tunDevice,
		"sudo ip -6 route replace "+testVMv6+"/128 dev "+tunDevice,
		"- sudo nft list chain inet atlas forward",
		"sudo nft add rule inet atlas forward ip6 daddr "+testVMv6+" oifname "+tunDevice+" accept",
		"- sudo nft list chain inet atlas forward",
		"sudo nft add rule inet atlas forward ip6 saddr "+testVMv6+" iifname "+tunDevice+" accept",
		"ip -j -6 route show default",
		"sudo ip -6 neigh replace proxy "+testVMv6+" dev eth0",
	)
}

// The proxy-NDP re-assert is unconditional and fails loud without an uplink, rather
// than silently skipping and black-holing ingress.
func TestSourceForwardFailsWithoutUplink(t *testing.T) {
	fake := newFakeCommands().exists("ip link show " + tunDevice) // no default route scripted
	if _, err := SourceForward(context.Background(), fake, testUUID, SourceForwardParams{VirtualMachineIPv6: testVMv6}); err == nil {
		t.Fatal("SourceForward succeeded with no uplink to answer NDP on")
	}
}

// A forward rule already in the chain is not re-added (no duplicate that would split a
// count).
func TestSourceForwardSkipsPresentRules(t *testing.T) {
	chain := "chain forward {\n" +
		"  ip6 daddr " + testVMv6 + " oifname " + tunDevice + " accept\n" +
		"  ip6 saddr " + testVMv6 + " iifname " + tunDevice + " accept\n}"
	fake := newFakeCommands().
		exists("ip link show "+tunDevice).
		output("sudo nft list chain inet atlas forward", chain).
		output("ip -j -6 route show default", `[{"dev":"eth0"}]`)

	if _, err := SourceForward(context.Background(), fake, testUUID, SourceForwardParams{VirtualMachineIPv6: testVMv6}); err != nil {
		t.Fatalf("SourceForward: %v", err)
	}
	assertNotIssued(t, fake, "nft add rule")
}

func TestSourceForwardRefusesTunnelDown(t *testing.T) {
	fake := newFakeCommands() // tunnel not up
	if _, err := SourceForward(context.Background(), fake, testUUID, SourceForwardParams{VirtualMachineIPv6: testVMv6}); err == nil {
		t.Fatal("SourceForward ran without the tunnel up")
	}
	assertNotIssued(t, fake, "ip -6 route replace")
}

func TestTargetReceive(t *testing.T) {
	fake := newFakeCommands().exists("ip link show " + tunDevice)

	result, err := TargetReceive(context.Background(), fake, testUUID, TargetReceiveParams{VirtualMachineIPv6: testVMv6})
	if err != nil {
		t.Fatalf("TargetReceive: %v", err)
	}
	if !result.Receiving {
		t.Error("TargetReceive did not report receiving")
	}

	assertTrace(t, fake,
		"? ip link show "+tunDevice,
		"sudo ip -6 route replace default dev "+tunDevice+" table 50688",
		"- ip -6 rule show",
		"sudo ip -6 rule add from "+testVMv6+" lookup 50688 priority 100",
	)
}

// The policy rule stacks, so a present one is not re-added.
func TestTargetReceiveSkipsPresentRule(t *testing.T) {
	fake := newFakeCommands().
		exists("ip link show "+tunDevice).
		output("ip -6 rule show", "100:\tfrom "+testVMv6+" lookup 50688\n")

	if _, err := TargetReceive(context.Background(), fake, testUUID, TargetReceiveParams{VirtualMachineIPv6: testVMv6}); err != nil {
		t.Fatalf("TargetReceive: %v", err)
	}
	assertNotIssued(t, fake, "ip -6 rule add")
}

func TestWithdrawPrivate(t *testing.T) {
	// An empty address is a clean no-op — a tenant-less VM's cutover calls it blind.
	if err := withdrawPrivate("", func(string) error { return errors.New("must not be called") }); err != nil {
		t.Errorf("empty address should be a no-op, got %v", err)
	}
	// A present address is removed from the cache.
	var removed string
	if err := withdrawPrivate("fdaa:0:1::5", func(address string) error { removed = address; return nil }); err != nil {
		t.Fatalf("withdrawPrivate: %v", err)
	}
	if removed != "fdaa:0:1::5" {
		t.Errorf("removed %q, want the private /128", removed)
	}
}
