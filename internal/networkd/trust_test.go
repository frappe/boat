package networkd

import "testing"

// A known peer whose Meta verifies against its anchored key is admitted.
func TestNotifyAliveAcceptsEstablishedKey(t *testing.T) {
	daemon, _ := newTestDaemon(t)
	peerPrivate, peerPublic, _ := GenerateSigningKeypair()
	daemon.trust["host-a"] = peerPublic
	node := peerNode("host-a", "2001:db8::a", signedMeta(t, testWireGuardPublic, "fdaa:0:0:a::1", peerPrivate, peerPublic, ""))

	if err := daemon.NotifyAlive(node); err != nil {
		t.Fatalf("an anchored, correctly-signed peer must be admitted: %v", err)
	}
}

// A known peer self-asserting a DIFFERENT key than its anchored one is refused: wire
// self-rotation is the forge a compromised host would attempt.
func TestNotifyAliveRefusesWireSelfRotation(t *testing.T) {
	daemon, _ := newTestDaemon(t)
	_, anchoredPublic, _ := GenerateSigningKeypair()
	daemon.trust["host-a"] = anchoredPublic
	rotatedPrivate, rotatedPublic, _ := GenerateSigningKeypair()
	node := peerNode("host-a", "2001:db8::a", signedMeta(t, testWireGuardPublic, "fdaa:0:0:a::1", rotatedPrivate, rotatedPublic, ""))

	if err := daemon.NotifyAlive(node); err == nil {
		t.Fatal("a peer self-asserting a different signing key must be refused")
	}
}

// An unknown peer with an operator key configured but NO introduction certificate is
// refused.
func TestNotifyAliveRefusesUnknownWithoutIntroduction(t *testing.T) {
	daemon, _ := newTestDaemon(t)
	_, operatorPublic, _ := GenerateSigningKeypair()
	daemon.operatorPublicKey = operatorPublic
	peerPrivate, peerPublic, _ := GenerateSigningKeypair()
	node := peerNode("host-a", "2001:db8::a", signedMeta(t, testWireGuardPublic, "fdaa:0:0:a::1", peerPrivate, peerPublic, ""))

	if err := daemon.NotifyAlive(node); err == nil {
		t.Fatal("an unknown peer with no introduction certificate must be refused")
	}
}

// An unknown peer carrying a valid operator introduction certificate is admitted and
// TOFU-persisted into both the runtime trust dir and the durable state.
func TestNotifyAliveAcceptsIntroductionAndTOFUs(t *testing.T) {
	daemon, _ := newTestDaemon(t)
	operatorPrivate, operatorPublic, _ := GenerateSigningKeypair()
	daemon.operatorPublicKey = operatorPublic
	peerPrivate, peerPublic, _ := GenerateSigningKeypair()
	introduction, err := SignIntroduction(map[string]any{
		"host_id":            "host-a",
		"signing_public_key": peerPublic,
		"generation":         introductionGeneration,
	}, operatorPrivate)
	if err != nil {
		t.Fatalf("SignIntroduction: %v", err)
	}
	node := peerNode("host-a", "2001:db8::a", signedMeta(t, testWireGuardPublic, "fdaa:0:0:a::1", peerPrivate, peerPublic, introduction))

	if err := daemon.NotifyAlive(node); err != nil {
		t.Fatalf("a valid introduction certificate must be accepted: %v", err)
	}
	if daemon.trust["host-a"] != peerPublic {
		t.Fatal("an introduced peer's key must be TOFU-learned into the trust directory")
	}
	if daemon.state.SigningPubkeys()["host-a"] != peerPublic {
		t.Fatal("an introduced peer's key must be TOFU-persisted into the durable state")
	}
}

// The injection guard runs in the admission gate: a peer whose Meta carries a newline
// in mesh_address is refused before it can reach a render.
func TestNotifyAliveRefusesInjectionInMeta(t *testing.T) {
	daemon, _ := newTestDaemon(t)
	peerPrivate, peerPublic, _ := GenerateSigningKeypair()
	daemon.trust["host-a"] = peerPublic
	node := peerNode("host-a", "2001:db8::a", signedMeta(t, testWireGuardPublic, "fdaa:0:0:a::1\n[Peer]", peerPrivate, peerPublic, ""))

	if err := daemon.NotifyAlive(node); err == nil {
		t.Fatal("a peer with a newline in mesh_address must be refused")
	}
}

// Our own alive message (name == our host_id), emitted by memberlist during setAlive,
// is always accepted.
func TestNotifyAliveAcceptsSelf(t *testing.T) {
	daemon, _ := newTestDaemon(t)
	node := peerNode(daemon.identity.HostID, daemon.identity.Endpoint, daemon.NodeMeta(512))
	if err := daemon.NotifyAlive(node); err != nil {
		t.Fatalf("our own alive message must be accepted: %v", err)
	}
}
