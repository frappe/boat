package migration

import (
	"context"
	"testing"
)

const (
	tunDevice = "mig6-3f2504e0"
	tunUnit   = "atlas-mig6-16165"

	// The socat systemd-run line the source lays down pre-cutover: the ExecStartPost
	// re-lays addr + MTU on every (re)start, and the endpoint LISTENS.
	sourceSystemdRun = "sudo systemd-run --unit=" + tunUnit +
		" --property=Type=simple --property=Restart=always --property=RestartSec=2" +
		" --property=ExecStartPost=/bin/sh -c 'for i in $(seq 50); do ip link show " + tunDevice +
		" >/dev/null 2>&1 && break; sleep 0.1; done; ip -6 addr replace fe80::a/64 dev " + tunDevice +
		" nodad; ip link set " + tunDevice + " mtu 1280 up'" +
		" -- socat TUN,tun-name=" + tunDevice + ",iff-up,iff-no-pi" +
		" TCP-LISTEN:16165,bind=0.0.0.0,reuseaddr,keepalive,keepidle=10,keepintvl=4,keepcnt=5"
)

func TestForwardUpSourcePreCutover(t *testing.T) {
	fake := newFakeCommands().exists("ip link show " + tunDevice)

	result, err := ForwardUp(context.Background(), fake, testUUID, ForwardUpParams{Role: "source"})
	if err != nil {
		t.Fatalf("ForwardUp: %v", err)
	}
	if result.TunnelDevice != tunDevice || !result.Up {
		t.Errorf("result = %+v", result)
	}

	assertTrace(t, fake,
		"- sudo sysctl -q -w net.ipv6.conf.all.forwarding=1",
		"- sudo systemctl is-active "+tunUnit,
		"- sudo systemctl stop "+tunUnit,
		"- sudo systemctl reset-failed "+tunUnit,
		sourceSystemdRun,
		"? ip link show "+tunDevice,
		"sudo ip -6 addr replace fe80::a/64 dev "+tunDevice+" nodad",
		"sudo ip link set "+tunDevice+" mtu 1280 up",
	)
}

// The target CONNECTS to the source's address, and addresses the tun with the other
// link-local end.
func TestForwardUpTargetConnects(t *testing.T) {
	fake := newFakeCommands().exists("ip link show " + tunDevice)
	_, err := ForwardUp(context.Background(), fake, testUUID, ForwardUpParams{Role: "target", SourceHost: testSource})
	if err != nil {
		t.Fatalf("ForwardUp: %v", err)
	}
	assertIssued(t, fake, "-- socat TUN,tun-name="+tunDevice+",iff-up,iff-no-pi TCP:"+testSource+":16165,retry=5,forever,keepalive,keepidle=10,keepintvl=4,keepcnt=5")
	assertIssued(t, fake, "sudo ip -6 addr replace fe80::b/64 dev "+tunDevice+" nodad")
}

func TestForwardUpRejectsBadArgs(t *testing.T) {
	if _, err := ForwardUp(context.Background(), newFakeCommands(), testUUID, ForwardUpParams{Role: "sideways"}); err == nil {
		t.Error("accepted an invalid role")
	}
	if _, err := ForwardUp(context.Background(), newFakeCommands(), testUUID, ForwardUpParams{Role: "target"}); err == nil {
		t.Error("accepted a target with no source host")
	}
}

// A bare-tunnel re-entry (no route args) whose socat is already active leaves the
// carrier alone — no stop, no re-lay — and only re-asserts the address.
func TestForwardUpSkipsRelayWhenActiveAndBare(t *testing.T) {
	fake := newFakeCommands().
		output("sudo systemctl is-active "+tunUnit, "active\n").
		exists("ip link show " + tunDevice)

	if _, err := ForwardUp(context.Background(), fake, testUUID, ForwardUpParams{Role: "source"}); err != nil {
		t.Fatalf("ForwardUp: %v", err)
	}
	assertNotIssued(t, fake, "systemd-run")
	assertNotIssued(t, fake, "systemctl stop")
	// The address is still re-asserted.
	assertIssued(t, fake, "sudo ip -6 addr replace fe80::a/64 dev "+tunDevice+" nodad")
}

// At cutover the route args arrive, so even an already-active unit is re-laid with the
// delivery route baked into its restart hook.
func TestForwardUpRelaysAtCutover(t *testing.T) {
	fake := newFakeCommands().
		output("sudo systemctl is-active "+tunUnit, "active\n").
		exists("ip link show " + tunDevice)

	if _, err := ForwardUp(context.Background(), fake, testUUID, ForwardUpParams{Role: "source", VirtualMachineIPv6: testVMv6}); err != nil {
		t.Fatalf("ForwardUp: %v", err)
	}
	// The unit is re-laid and the source's /128 delivery route is in the ExecStartPost.
	assertIssued(t, fake, "systemd-run")
	assertIssued(t, fake, "ip -6 route replace "+testVMv6+"/128 dev "+tunDevice)
}

func TestForwardDownSource(t *testing.T) {
	chain := "table inet atlas {\n" +
		"  chain forward {\n" +
		"    ip6 daddr " + testVMv6 + " oifname \"" + tunDevice + "\" accept # handle 12\n" +
		"    ip6 saddr " + testVMv6 + " iifname \"" + tunDevice + "\" accept # handle 13\n" +
		"  }\n}"
	fake := newFakeCommands().
		output("sudo nft -a list chain inet atlas forward", chain).
		output("ip -j -6 route show default", `[{"dst":"default","dev":"eth0"}]`)

	result, err := ForwardDown(context.Background(), fake, testUUID, ForwardDownParams{Role: "source", VirtualMachineIPv6: testVMv6})
	if err != nil {
		t.Fatalf("ForwardDown: %v", err)
	}
	if !result.Down {
		t.Error("ForwardDown did not report done")
	}

	assertTrace(t, fake,
		"- sudo ip -6 route del "+testVMv6+"/128 dev "+tunDevice,
		"- sudo nft -a list chain inet atlas forward",
		"- sudo nft delete rule inet atlas forward handle 12",
		"- sudo nft delete rule inet atlas forward handle 13",
		"- ip -j -6 route show default",
		"- sudo ip -6 neigh del proxy "+testVMv6+" dev eth0",
		"- sudo systemctl stop "+tunUnit,
		"- sudo systemctl reset-failed "+tunUnit,
		"- sudo ip link del "+tunDevice,
	)
}

func TestForwardDownTarget(t *testing.T) {
	fake := newFakeCommands()
	if _, err := ForwardDown(context.Background(), fake, testUUID, ForwardDownParams{Role: "target", VirtualMachineIPv6: testVMv6}); err != nil {
		t.Fatalf("ForwardDown: %v", err)
	}
	assertTrace(t, fake,
		"- sudo ip -6 rule del from "+testVMv6+" lookup 50688 priority 100",
		"- sudo ip -6 route del default dev "+tunDevice+" table 50688",
		"- sudo systemctl stop "+tunUnit,
		"- sudo systemctl reset-failed "+tunUnit,
		"- sudo ip link del "+tunDevice,
	)
}
