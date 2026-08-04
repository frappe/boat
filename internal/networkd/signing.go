// ed25519 signatures for ANCP records, ported from signing.py (spec §19).
//
// Two of signing.py's three checks survive the memberlist cutover:
//
//   - Per-record signature (§19.3) — Sign/Verify with kind "membership"|"ownership".
//     Every MembershipRecord and OwnershipAdvertisement is signed by its ORIGIN's
//     key, independent of the relay that forwarded it, so a compromised relay cannot
//     fabricate or mutate a record "from" another origin. This is the check that
//     matters most: memberlist's optional AES keyring authenticates "a cluster
//     member sent this", but does NOT bind a specific origin to a record.
//   - Introduction certificate (§19.5) — SignIntroduction/VerifyIntroduction — the
//     operator-signed binding {host_id, signing_public_key, generation} a newcomer
//     carries on first contact, plus SignDetached/VerifyDetached over the exact seed
//     bytes (§19.4). Used by the parent's trust.go / seed.go.
//
// The envelope signature (§19.1) is GONE: memberlist authenticates the datagram
// sender via its transport and optional AES keyring, so there is no whole-datagram
// signature to port.
//
// HARD CUTOVER — there is no Python interop at runtime, so this signing only needs
// to be SELF-CONSISTENT (Go signs, Go verifies); it does NOT reproduce Python's wire
// bytes. The canonical body is Go's encoding/json over a map[string]any, which
// alphabetizes keys and emits compact separators — the same shape signing.py built
// with `json.dumps(sort_keys=True, separators=(",",":"))`. HTML-escaping and other
// encoder details need only agree with themselves, and they do because the same
// encoder runs on both the sign and the verify side. Keys are stored as base64 of
// the raw 32-byte seed (private) and the raw 32-byte public key, matching the
// on-disk key files keys.go writes.
package networkd

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrSignature is the sentinel every verification failure wraps — a missing or
// non-base64 signature, a wrong key, a tampered body, or a malformed pubkey. The
// gossip apply path drops the record and increments a counter on errors.Is(err,
// ErrSignature); it never silently accepts. Mirrors signing.py's SignatureError.
var ErrSignature = errors.New("record signature verification failed")

// The domain-separation tags. A signature over one kind can never be replayed on
// another because the tag is folded into the signed body under the "_kind" key —
// the defence signing.py documents against a future relay bug reusing a membership
// signature on an ownership advertisement.
const (
	kindMembership   = "membership"
	kindOwnership    = "ownership"
	kindIntroduction = "introduction"
)

// SignMembership signs a Membership Record with the origin's ed25519 signing key,
// returning the base64 signature the wire serializer carries alongside the record.
func SignMembership(record MembershipRecord, signingPrivateBase64 string) (string, error) {
	return sign(membershipBody(record), kindMembership, signingPrivateBase64)
}

// VerifyMembership checks signature against the origin's published signing pubkey.
// Returns nil only if the record's body is intact and the signature is the origin's.
func VerifyMembership(record MembershipRecord, signature, signingPublicBase64 string) error {
	return verify(membershipBody(record), signature, kindMembership, signingPublicBase64)
}

// SignOwnership signs an Ownership Advertisement with the origin's signing key.
func SignOwnership(advertisement OwnershipAdvertisement, signingPrivateBase64 string) (string, error) {
	return sign(ownershipBody(advertisement), kindOwnership, signingPrivateBase64)
}

// VerifyOwnership checks the advertisement's own Signature field against the
// origin's published signing pubkey — the check the apply path runs before storing.
func VerifyOwnership(advertisement OwnershipAdvertisement, signingPublicBase64 string) error {
	return verify(ownershipBody(advertisement), advertisement.Signature, kindOwnership, signingPublicBase64)
}

// SignIntroduction signs the newcomer's identity binding {host_id,
// signing_public_key, generation} with the OPERATOR's provision private key (§19.5).
// The result rides the newcomer's first contact; existing hosts verify it against
// the operator's provision pubkey seeded to every host.
func SignIntroduction(body map[string]any, operatorPrivateBase64 string) (string, error) {
	return sign(body, kindIntroduction, operatorPrivateBase64)
}

// VerifyIntroduction checks an introduction certificate against the operator pubkey.
func VerifyIntroduction(body map[string]any, signature, operatorPublicBase64 string) error {
	return verify(body, signature, kindIntroduction, operatorPublicBase64)
}

// SignDetached signs the EXACT bytes with an ed25519 private key — no canonical-dict
// shaping and no _kind tag, unlike the record signers. The signer commits to the
// literal body the verifier re-reads, which is what the operator-signed seed file
// needs (§19.4): the seed is the sole trust root and is signed as its on-disk bytes
// so controller and host agree byte-for-byte.
func SignDetached(body []byte, signingPrivateBase64 string) (string, error) {
	private, err := loadPrivate(signingPrivateBase64)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(private, body)), nil
}

// VerifyDetached verifies a SignDetached signature over the exact body bytes.
func VerifyDetached(body []byte, signature, signingPublicBase64 string) error {
	return verifyRaw(body, signature, signingPublicBase64)
}

// GenerateSigningKeypair mints a fresh ed25519 keypair, returning both halves
// base64-encoded: the private is the raw 32-byte seed, the public the raw 32-byte
// key. keys.go base64s nothing further — it writes these strings straight to disk.
func GenerateSigningKeypair() (privateBase64, publicBase64 string, err error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	seed := base64.StdEncoding.EncodeToString(private.Seed())
	return seed, base64.StdEncoding.EncodeToString(public), nil
}

// membershipBody is the canonical dict a membership signature commits to — every
// field but the signature, keyed the way signing.py's payload was so the shape is
// self-documenting. json.Marshal alphabetizes the keys, so field order here is
// irrelevant to the signed bytes.
func membershipBody(record MembershipRecord) map[string]any {
	return map[string]any{
		"host_id":            record.HostID,
		"kind":               string(record.Kind),
		"state":              string(record.State),
		"endpoint":           record.Endpoint,
		"wg_public_key":      record.WireGuardPublicKey,
		"mesh_address":       record.MeshAddress,
		"generation":         record.Generation,
		"signing_public_key": record.SigningPublicKey,
	}
}

// ownershipBody is the canonical dict an ownership signature commits to — origin,
// generation and the sorted owned set (Owned is already sorted and de-duplicated).
func ownershipBody(advertisement OwnershipAdvertisement) map[string]any {
	return map[string]any{
		"origin":     advertisement.Origin,
		"generation": advertisement.Generation,
		"owned":      advertisement.Owned,
	}
}

// sign canonicalizes body with the _kind domain tag and returns the base64 ed25519
// signature.
func sign(body map[string]any, kind, signingPrivateBase64 string) (string, error) {
	canonical, err := canonicalBody(body, kind)
	if err != nil {
		return "", err
	}
	private, err := loadPrivate(signingPrivateBase64)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(private, canonical)), nil
}

// verify reconstructs the same canonical bytes the signer committed to and checks
// the signature against the origin's pubkey.
func verify(body map[string]any, signature, kind, signingPublicBase64 string) error {
	canonical, err := canonicalBody(body, kind)
	if err != nil {
		return err
	}
	return verifyRaw(canonical, signature, signingPublicBase64)
}

// verifyRaw is the shared last mile: decode the signature and pubkey, run the
// ed25519 check, and wrap every failure in ErrSignature so a caller need only test
// errors.Is. Used by both the canonical-body path and the detached path.
func verifyRaw(message []byte, signature, signingPublicBase64 string) error {
	if signature == "" {
		return fmt.Errorf("%w: no signature", ErrSignature)
	}
	raw, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("%w: signature is not base64: %v", ErrSignature, err)
	}
	public, err := loadPublic(signingPublicBase64)
	if err != nil {
		return err
	}
	if !ed25519.Verify(public, message, raw) {
		return fmt.Errorf("%w: invalid ed25519 signature", ErrSignature)
	}
	return nil
}

// canonicalBody is the bytes both sides commit to: body with the signature stripped
// (callers never put one in) and the _kind domain separator folded in, encoded with
// alphabetized keys and compact separators. HTML-escaping is left at encoding/json's
// default because the same encoder runs on both sign and verify (self-consistent).
func canonicalBody(body map[string]any, kind string) ([]byte, error) {
	withKind := make(map[string]any, len(body)+1)
	for key, value := range body {
		if key == "signature" {
			continue
		}
		withKind[key] = value
	}
	withKind["_kind"] = kind
	return json.Marshal(withKind)
}

// loadPrivate decodes a base64 raw 32-byte seed into an ed25519 private key.
func loadPrivate(privateBase64 string) (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(privateBase64)
	if err != nil {
		return nil, fmt.Errorf("%w: private key is not base64: %v", ErrSignature, err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%w: private key seed is %d bytes, want %d", ErrSignature, len(seed), ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// loadPublic decodes a base64 raw 32-byte ed25519 public key. A malformed pubkey is
// surfaced loud, never silently accepted.
func loadPublic(publicBase64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(publicBase64)
	if err != nil {
		return nil, fmt.Errorf("%w: public key is not base64: %v", ErrSignature, err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: public key is %d bytes, want %d", ErrSignature, len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}
