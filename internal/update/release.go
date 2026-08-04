// Package update is Boat updating itself under live VMs (spec/33 §5). The daemon
// never polls: Atlas holds the desired version (Server.boat_version) and pushes a
// signed release, because only Atlas knows which hosts are Unknown or mid-operation
// and can therefore stagger a rollout without ever updating the whole fleet at once
// — the one failure mode that bricks every host together.
//
// This file is the part that decides whether a pushed release may be trusted at
// all, and it is deliberately pure: no host, no subprocess, no clock. A release is
// a binary, the version it claims to be, and an ed25519 signature over a manifest
// that binds the two. Verification is the first of §5's seven steps ("verify
// signature and checksum before anything else") and it is the one step whose
// failure must stop the update dead — an unverified binary is never renamed over
// /usr/local/bin/boat. install.go does the renaming; this file is what it asks
// first.
package update

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// Errors verification can return. They are sentinels so a caller — and the
// §5 step-5 health path that rolls back — can tell "the bytes were tampered with"
// (ErrChecksum) apart from "the signer is not who we trust" (ErrSignature) apart
// from "the manifest is malformed" (ErrManifest), because those three are a
// corrupted download, an attack, and a bad publisher respectively.
var (
	ErrChecksum  = errors.New("update: binary does not match its manifest checksum")
	ErrSignature = errors.New("update: manifest signature is not from the trusted key")
	ErrManifest  = errors.New("update: manifest is malformed")
)

// Manifest is what the trusted key signs: the version a release claims to be and
// the SHA-256 of its bytes. Binding BOTH into one signed message is load-bearing —
// a signature over the checksum alone could be lifted onto a different version
// number, and a signature over the version alone would not pin the bytes. The
// signed message is therefore the two together, in the one canonical form
// canonicalManifest renders.
type Manifest struct {
	Version string
	SHA256  string // lowercase hex of the release binary's SHA-256
}

// Release is a manifest, the bytes it describes, and the detached signature over
// the manifest. Atlas hands one of these to POST /v1/update.
type Release struct {
	Manifest  Manifest
	Binary    []byte
	Signature []byte
}

// canonicalManifest is the exact byte string the publisher signs and the host
// verifies. It must be identical on both sides down to the trailing newline, so it
// is defined once, here, and never assembled ad hoc at a call site. Version first,
// then checksum, each on its own line: a field that could contain the separator
// cannot forge a second field, because a version carrying a newline fails
// validateManifest before it reaches here.
func canonicalManifest(manifest Manifest) []byte {
	return []byte(fmt.Sprintf("boat-release\nversion=%s\nsha256=%s\n", manifest.Version, manifest.SHA256))
}

// Verify is §5 step 2's gate. It returns nil only when the trusted key signed this
// exact (version, checksum) manifest AND the binary hashes to that checksum — the
// signature authenticates the claim, the checksum authenticates the bytes, and an
// update proceeds on neither alone.
//
// The order is chosen so the error is the true one: the signature is checked first,
// so a binary whose signer is not trusted reports ErrSignature rather than being
// judged on a checksum nobody vouched for; only once the manifest is authentic is
// the binary hashed and compared to it.
func Verify(release Release, trusted ed25519.PublicKey) error {
	if err := validateManifest(release.Manifest); err != nil {
		return err
	}
	if len(trusted) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: trusted key is not an ed25519 public key", ErrSignature)
	}
	if !ed25519.Verify(trusted, canonicalManifest(release.Manifest), release.Signature) {
		return ErrSignature
	}
	sum := sha256.Sum256(release.Binary)
	if hex.EncodeToString(sum[:]) != release.Manifest.SHA256 {
		return ErrChecksum
	}
	return nil
}

// validateManifest refuses a manifest whose fields could break the canonical
// encoding or name nothing. A version or checksum carrying a newline could forge a
// second manifest line, so both are rejected before they reach canonicalManifest;
// an empty version is a release that names no version, which the desired/running
// comparison could never satisfy.
func validateManifest(manifest Manifest) error {
	if manifest.Version == "" {
		return fmt.Errorf("%w: empty version", ErrManifest)
	}
	if len(manifest.SHA256) != 2*sha256.Size {
		return fmt.Errorf("%w: checksum is not a SHA-256 hex digest", ErrManifest)
	}
	if _, err := hex.DecodeString(manifest.SHA256); err != nil {
		return fmt.Errorf("%w: checksum is not hex", ErrManifest)
	}
	for _, field := range []string{manifest.Version, manifest.SHA256} {
		for _, r := range field {
			if r == '\n' || r == '\r' {
				return fmt.Errorf("%w: field contains a newline", ErrManifest)
			}
		}
	}
	return nil
}

// ShouldApply is the desired-versus-running decision (§5 step 1). Boat updates when
// Atlas's desired version differs from what is running — versions are git-describe
// strings, not orderable semver, so "different" is the whole test and a downgrade
// (a rollout being pulled back) is as valid as an upgrade. An empty desired means
// Atlas has asserted nothing and the host stays put.
func ShouldApply(running, desired string) bool {
	return desired != "" && running != desired
}
