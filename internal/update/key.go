package update

// key.go loads the ONE public key a host trusts to sign its updates. It is a
// separate file from release.go because Verify is pure — it takes the key as an
// argument and touches no filesystem — and this is the impure edge that reads a
// key off disk. Keeping them apart is what lets the verification logic stay
// testable with a fixed in-memory key while the daemon and the updater both load
// the real one from a configurable path.
//
// The key is NEVER hardcoded. A build with a signer baked into it is a build that
// cannot rotate the signer without a re-release, and a leaked signing key would
// then have no revocation short of shipping a new binary to every host — the
// exact bricks-the-fleet-together failure the staged-rollout design exists to
// avoid. So the trusted key is a file the operator provisions (root-owned,
// world-readable is fine: a public key is public), named by --update-key-file on
// both `boat daemon` and `boat update-apply`, and an absent file means self-
// update is simply not enabled on this host rather than that some default key is.

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
)

// LoadTrustedKey reads an ed25519 public key from path, as lowercase or uppercase
// hex — the same framing the manifest's sha256 uses, so an operator provisioning
// a host reads one encoding across the whole update surface. Surrounding
// whitespace (a trailing newline from `echo`) is tolerated; anything else is
// refused, because a key that decodes to the wrong length would fail every
// verification with ErrSignature and look like a bad signer rather than a bad
// key file.
//
// A missing file is reported as os.ErrNotExist unwrapped, so the caller can tell
// "no key configured, self-update disabled" apart from "a key was configured but
// is malformed" — the first is a policy, the second is an operator error.
func LoadTrustedKey(path string) (ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoded := make([]byte, hex.DecodedLen(len(bytes.TrimSpace(raw))))
	n, err := hex.Decode(decoded, bytes.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("the update key in %s is not hex: %w", path, err)
	}
	if n != ed25519.PublicKeySize {
		return nil, fmt.Errorf(
			"the update key in %s is %d bytes, want a %d-byte ed25519 public key",
			path, n, ed25519.PublicKeySize,
		)
	}
	return ed25519.PublicKey(decoded[:n]), nil
}
