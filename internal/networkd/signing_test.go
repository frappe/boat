package networkd

import (
	"errors"
	"testing"
)

// signingPair mints a fresh ed25519 keypair for a test, failing the test on error.
func signingPair(t *testing.T) (private, public string) {
	t.Helper()
	private, public, err := GenerateSigningKeypair()
	if err != nil {
		t.Fatalf("GenerateSigningKeypair: %v", err)
	}
	return private, public
}

func sampleMembership() MembershipRecord {
	return MembershipRecord{
		HostID: "host-a", Kind: MembershipKindMember, State: MemberStateAlive,
		Endpoint: "2001:db8::1", WireGuardPublicKey: testWireGuardPublic,
		MeshAddress: "fdaa:0:0:a::1", Generation: 4, SigningPublicKey: "unused-in-body-check",
	}
}

// A membership signed by the origin verifies against the origin's public key.
func TestMembershipSignatureRoundTrips(t *testing.T) {
	private, public := signingPair(t)
	record := sampleMembership()
	signature, err := SignMembership(record, private)
	if err != nil {
		t.Fatalf("SignMembership: %v", err)
	}
	if err := VerifyMembership(record, signature, public); err != nil {
		t.Fatalf("VerifyMembership: %v", err)
	}
}

// Any tamper of the signed body — here the endpoint — invalidates the signature.
func TestMembershipSignatureCatchesTamper(t *testing.T) {
	private, public := signingPair(t)
	record := sampleMembership()
	signature, err := SignMembership(record, private)
	if err != nil {
		t.Fatal(err)
	}
	tampered := record
	tampered.Endpoint = "2001:db8::evil"
	if err := VerifyMembership(tampered, signature, public); !errors.Is(err, ErrSignature) {
		t.Fatalf("a tampered body must fail with ErrSignature, got %v", err)
	}
}

// A signature made by one origin does not verify under another origin's key — the
// §19.3 forgery defence.
func TestMembershipSignatureRejectsWrongKey(t *testing.T) {
	private, _ := signingPair(t)
	_, otherPublic := signingPair(t)
	record := sampleMembership()
	signature, err := SignMembership(record, private)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyMembership(record, signature, otherPublic); !errors.Is(err, ErrSignature) {
		t.Fatalf("verifying under the wrong key must fail with ErrSignature, got %v", err)
	}
}

// The _kind domain separator stops a signature over one kind being replayed on
// another, even with an identical body.
func TestDomainSeparationBindsSignatureToKind(t *testing.T) {
	private, public := signingPair(t)
	body := map[string]any{"origin": "host-a", "generation": Generation(1)}
	signature, err := sign(body, kindMembership, private)
	if err != nil {
		t.Fatal(err)
	}
	if err := verify(body, signature, kindOwnership, public); !errors.Is(err, ErrSignature) {
		t.Fatalf("a membership signature must not verify as ownership, got %v", err)
	}
}

// An ownership advertisement round-trips through its own Signature field, and a
// changed owned set is caught.
func TestOwnershipSignatureRoundTripsAndCatchesTamper(t *testing.T) {
	private, public := signingPair(t)
	advertisement := OwningAdvertisement("host-a", 2, []IP6{"fdab::1", "fdab::2"})
	signature, err := SignOwnership(advertisement, private)
	if err != nil {
		t.Fatal(err)
	}
	advertisement.Signature = signature
	if err := VerifyOwnership(advertisement, public); err != nil {
		t.Fatalf("VerifyOwnership: %v", err)
	}
	tampered := OwningAdvertisement("host-a", 2, []IP6{"fdab::1", "fdab::3"})
	tampered.Signature = signature
	if err := VerifyOwnership(tampered, public); !errors.Is(err, ErrSignature) {
		t.Fatalf("a changed owned set must fail with ErrSignature, got %v", err)
	}
}

// The detached signer commits to the exact bytes — no canonicalization — as the seed
// file needs.
func TestDetachedSignatureOverExactBytes(t *testing.T) {
	private, public := signingPair(t)
	body := []byte(`{"seed":"exact bytes, spaces and all"}`)
	signature, err := SignDetached(body, private)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyDetached(body, signature, public); err != nil {
		t.Fatalf("VerifyDetached: %v", err)
	}
	if err := VerifyDetached(append(body, ' '), signature, public); !errors.Is(err, ErrSignature) {
		t.Fatalf("one extra byte must fail with ErrSignature, got %v", err)
	}
}

// A missing signature is a verification failure, not a silent accept.
func TestVerifyRejectsMissingSignature(t *testing.T) {
	_, public := signingPair(t)
	if err := VerifyMembership(sampleMembership(), "", public); !errors.Is(err, ErrSignature) {
		t.Fatalf("an empty signature must fail with ErrSignature, got %v", err)
	}
}
