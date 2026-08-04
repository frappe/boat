package networkd

import "testing"

// NotifyJoin applies a peer's membership from its Meta + advertise address, so it
// renders and its verified /128s route to it.
func TestNotifyJoinAppliesMembership(t *testing.T) {
	daemon, _ := newTestDaemon(t)
	peerPrivate, peerPublic, _ := GenerateSigningKeypair()
	node := peerNode("host-a", "2001:db8::a", signedMeta(t, testWireGuardPublic, "fdaa:0:0:a::1", peerPrivate, peerPublic, ""))

	daemon.NotifyJoin(node)

	record, present := daemon.state.RenderMembers()["host-a"]
	if !present {
		t.Fatal("a joined peer must be in the render set")
	}
	if record.Endpoint != "2001:db8::a" || record.WireGuardPublicKey != testWireGuardPublic {
		t.Fatalf("membership record built wrong: %+v", record)
	}
}

// The ownership_grace nuance memberlist will not do: on leave a peer keeps rendering
// from routable_dead until now-deadAt ≥ ownership_grace, then is reaped; a rejoin
// clears the timer.
func TestNotifyLeaveRetainsRoutesUntilGraceThenReaps(t *testing.T) {
	daemon, clock := newTestDaemon(t)
	peerPrivate, peerPublic, _ := GenerateSigningKeypair()
	node := peerNode("host-a", "2001:db8::a", signedMeta(t, testWireGuardPublic, "fdaa:0:0:a::1", peerPrivate, peerPublic, ""))
	daemon.NotifyJoin(node)
	daemon.trust["host-a"] = peerPublic
	daemon.applyOwnershipFromWire(signedOwnership(t, "host-a", 1, []IP6{"fdab::a1"}, peerPrivate))

	*clock = 100
	daemon.NotifyLeave(node)
	if _, pending := daemon.deadAt["host-a"]; !pending {
		t.Fatal("NotifyLeave must stamp the ownership_grace clock")
	}
	if _, rendered := daemon.state.RenderMembers()["host-a"]; !rendered {
		t.Fatal("a left peer must still render its [Peer] during the grace window")
	}

	// Inside the grace window (30s < 60s): nothing is reaped.
	*clock = 130
	daemon.mu.Lock()
	reaped := daemon.reapDeadOriginsLocked(*clock)
	daemon.mu.Unlock()
	if reaped {
		t.Fatal("ownership must survive inside the grace window")
	}
	if _, rendered := daemon.state.RenderMembers()["host-a"]; !rendered {
		t.Fatal("the [Peer] must still render inside the grace window")
	}

	// Past the grace window (61s ≥ 60s): reaped, [Peer] gone.
	*clock = 161
	daemon.mu.Lock()
	reaped = daemon.reapDeadOriginsLocked(*clock)
	daemon.mu.Unlock()
	if !reaped {
		t.Fatal("ownership must be reaped once the grace window elapses")
	}
	if _, rendered := daemon.state.RenderMembers()["host-a"]; rendered {
		t.Fatal("once reaped the dead peer's [Peer] must disappear")
	}
}

// A rejoin inside the grace window clears the timer so the host is not reaped.
func TestNotifyJoinClearsGraceTimerOnRejoin(t *testing.T) {
	daemon, clock := newTestDaemon(t)
	peerPrivate, peerPublic, _ := GenerateSigningKeypair()
	node := peerNode("host-a", "2001:db8::a", signedMeta(t, testWireGuardPublic, "fdaa:0:0:a::1", peerPrivate, peerPublic, ""))
	daemon.NotifyJoin(node)

	*clock = 100
	daemon.NotifyLeave(node)
	if _, pending := daemon.deadAt["host-a"]; !pending {
		t.Fatal("leave must stamp the grace timer")
	}

	*clock = 110
	daemon.NotifyJoin(node)
	if _, pending := daemon.deadAt["host-a"]; pending {
		t.Fatal("a rejoin must clear the grace timer")
	}
}
