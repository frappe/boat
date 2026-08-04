package networkd

import (
	"testing"

	"github.com/hashicorp/memberlist"
)

// NodeMeta emits a signed blob that verifies against our own signing key and fits
// comfortably under memberlist's 512-byte limit.
func TestNodeMetaSignsFitsAndVerifies(t *testing.T) {
	daemon, _ := newTestDaemon(t)
	blob := daemon.NodeMeta(memberlist.MetaMaxSize)
	if len(blob) == 0 {
		t.Fatal("NodeMeta returned an empty blob")
	}
	if len(blob) > memberlist.MetaMaxSize {
		t.Fatalf("NodeMeta blob is %d bytes, over the %d limit", len(blob), memberlist.MetaMaxSize)
	}
	meta, err := decodeNodeMeta(blob)
	if err != nil {
		t.Fatalf("decodeNodeMeta: %v", err)
	}
	if err := verifyMetaSignature(meta, daemon.signingPublicKey); err != nil {
		t.Fatalf("our own Meta must verify against our signing key: %v", err)
	}
}

// The ownership wire encoding round-trips through the canonical form, and a decoded
// advertisement still verifies against the origin's key.
func TestOwnershipWireRoundTripVerifies(t *testing.T) {
	private, public, err := GenerateSigningKeypair()
	if err != nil {
		t.Fatal(err)
	}
	advertisement := signedOwnership(t, "host-a", 3, []IP6{"fdab::2", "fdab::1"}, private)
	encoded, err := encodeOwnership(advertisement)
	if err != nil {
		t.Fatalf("encodeOwnership: %v", err)
	}
	decoded, err := decodeOwnership(encoded)
	if err != nil {
		t.Fatalf("decodeOwnership: %v", err)
	}
	if err := VerifyOwnership(decoded, public); err != nil {
		t.Fatalf("decoded advertisement must verify: %v", err)
	}
}

// An advertisement from an origin whose signing key we do not yet trust is DEFERRED —
// dropped, counted, never applied — so an authenticated relay cannot inject a phantom
// /128 nor advance the origin's generation past the authentic record.
func TestApplyOwnershipFromWireDefersUnknownOrigin(t *testing.T) {
	daemon, _ := newTestDaemon(t)
	private, _, _ := GenerateSigningKeypair()
	advertisement := signedOwnership(t, "host-x", 1, []IP6{"fdab::1"}, private)

	daemon.applyOwnershipFromWire(advertisement)

	if _, present := daemon.state.Ownership()["host-x"]; present {
		t.Fatal("an untrusted origin's advertisement must not be applied")
	}
	if daemon.counters.snapshot()["ownership_deferred_no_key"] != 1 {
		t.Fatalf("deferred counter = %v, want 1", daemon.counters.snapshot())
	}
}

// A trusted origin's valid advertisement is applied and schedules an apply.
func TestApplyOwnershipFromWireVerifiesAndApplies(t *testing.T) {
	daemon, _ := newTestDaemon(t)
	private, public, _ := GenerateSigningKeypair()
	daemon.trust["host-x"] = public
	advertisement := signedOwnership(t, "host-x", 1, []IP6{"fdab::1"}, private)

	daemon.applyOwnershipFromWire(advertisement)

	if _, present := daemon.state.Ownership()["host-x"]; !present {
		t.Fatal("a trusted origin's valid advertisement must be applied")
	}
	if daemon.applyDueAt == 0 {
		t.Fatal("applying an ownership change must schedule a debounced apply")
	}
}

// A trusted origin but a tampered signature is dropped and counted, never applied.
func TestApplyOwnershipFromWireRejectsBadSignature(t *testing.T) {
	daemon, _ := newTestDaemon(t)
	private, public, _ := GenerateSigningKeypair()
	daemon.trust["host-x"] = public
	advertisement := signedOwnership(t, "host-x", 1, []IP6{"fdab::1"}, private)
	advertisement.Signature = "AAAA" // corrupt

	daemon.applyOwnershipFromWire(advertisement)

	if _, present := daemon.state.Ownership()["host-x"]; present {
		t.Fatal("an advertisement with a bad signature must not be applied")
	}
	if daemon.counters.snapshot()["signature_failed"] != 1 {
		t.Fatalf("signature_failed counter = %v, want 1", daemon.counters.snapshot())
	}
}

// LocalState marshals the full ownership stream and MergeRemoteState applies every
// verified advertisement — the anti-entropy backstop, independent of broadcast.
func TestLocalStateAndMergeRemoteStateCarryOwnership(t *testing.T) {
	source, _ := newTestDaemon(t)
	private, public, _ := GenerateSigningKeypair()
	source.trust["host-x"] = public
	source.applyOwnershipFromWire(signedOwnership(t, "host-x", 5, []IP6{"fdab::9"}, private))

	stream := source.LocalState(true)

	sink, _ := newTestDaemon(t)
	sink.trust["host-x"] = public
	sink.MergeRemoteState(stream, true)

	got, present := sink.state.Ownership()["host-x"]
	if !present || got.Generation != 5 {
		t.Fatalf("merged ownership for host-x = %+v, want generation 5", got)
	}
}
