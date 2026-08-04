package networkd

import (
	"encoding/json"
	"net"
	"testing"

	"github.com/hashicorp/memberlist"
)

// newTestDaemon builds a Daemon wired for the pure delegate/event/trust logic — no
// memberlist handle, no host, a controllable clock — so the callbacks are exercised
// exactly as memberlist would drive them but with no gossip substrate.
func newTestDaemon(t *testing.T) (*Daemon, *float64) {
	t.Helper()
	signingPrivate, signingPublic, err := GenerateSigningKeypair()
	if err != nil {
		t.Fatalf("GenerateSigningKeypair: %v", err)
	}
	clock := new(float64)
	daemon := &Daemon{
		identity:           HostIdentity{HostID: "self", Endpoint: "2001:db8::1", MeshAddress: "fdaa:0:0:1::1"},
		config:             DefaultConfig(),
		wireGuardPublicKey: testWireGuardPublic,
		signingPrivateKey:  signingPrivate,
		signingPublicKey:   signingPublic,
		counters:           newCounters(),
		conflicts:          NewConflictTracker(""),
		state:              NewAppliedState(0),
		trust:              map[HostID]string{"self": signingPublic},
		peerGeneration:     map[HostID]Generation{},
		deadAt:             map[HostID]float64{},
		broadcasts:         &memberlist.TransmitLimitedQueue{NumNodes: func() int { return 1 }, RetransmitMult: 4},
	}
	daemon.nowFn = func() float64 { return *clock }
	return daemon, clock
}

// signedMeta builds the signed Meta blob a peer with this keypair would publish.
func signedMeta(t *testing.T, wireGuardPublic, meshAddress, signingPrivate, signingPublic, introduction string) []byte {
	t.Helper()
	body, err := canonicalMetaBody(wireGuardPublic, meshAddress, signingPublic)
	if err != nil {
		t.Fatalf("canonicalMetaBody: %v", err)
	}
	signature, err := SignDetached(body, signingPrivate)
	if err != nil {
		t.Fatalf("SignDetached: %v", err)
	}
	encoded, err := json.Marshal(nodeMeta{
		WireGuardPublicKey:    wireGuardPublic,
		MeshAddress:           meshAddress,
		SigningPublicKey:      signingPublic,
		IntroductionSignature: introduction,
		Signature:             signature,
	})
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	return encoded
}

// peerNode is a memberlist Node as the callbacks receive it: a name (host_id), an
// advertise address (the endpoint) and a Meta blob.
func peerNode(name, endpoint string, meta []byte) *memberlist.Node {
	return &memberlist.Node{Name: name, Addr: net.ParseIP(endpoint), Meta: meta}
}

// signedOwnership builds a signed OwnershipAdvertisement for origin.
func signedOwnership(t *testing.T, origin string, generation Generation, owned []IP6, signingPrivate string) OwnershipAdvertisement {
	t.Helper()
	advertisement := OwningAdvertisement(origin, generation, owned)
	signature, err := SignOwnership(advertisement, signingPrivate)
	if err != nil {
		t.Fatalf("SignOwnership: %v", err)
	}
	advertisement.Signature = signature
	return advertisement
}
