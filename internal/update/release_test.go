package update

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

// signedRelease builds a release the trusted key vouches for: the checksum of the
// bytes and a signature over the (version, checksum) manifest. A test that wants a
// tampered release mutates the value this returns, so every negative case starts
// from a genuinely valid one.
func signedRelease(key ed25519.PrivateKey, version string, binary []byte) Release {
	sum := sha256.Sum256(binary)
	manifest := Manifest{Version: version, SHA256: hex.EncodeToString(sum[:])}
	return Release{
		Manifest:  manifest,
		Binary:    binary,
		Signature: ed25519.Sign(key, canonicalManifest(manifest)),
	}
}

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	// A fixed seed keeps the test reproducible — the same key every run, no clock
	// or entropy, which is what lets this file assert on exact behaviour.
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	private := ed25519.NewKeyFromSeed(seed)
	return private.Public().(ed25519.PublicKey), private
}

func TestVerifyAcceptsAGenuineRelease(t *testing.T) {
	public, private := testKey(t)
	release := signedRelease(private, "v1.2.3", []byte("a boat binary"))
	if err := Verify(release, public); err != nil {
		t.Fatalf("a genuine release did not verify: %v", err)
	}
}

// A flipped byte after signing must be caught by the checksum, never installed.
func TestVerifyRejectsTamperedBytes(t *testing.T) {
	public, private := testKey(t)
	release := signedRelease(private, "v1.2.3", []byte("a boat binary"))
	release.Binary = []byte("a boat binary with a backdoor")
	if err := Verify(release, public); !errors.Is(err, ErrChecksum) {
		t.Fatalf("tampered bytes returned %v, want ErrChecksum", err)
	}
}

// A release signed by a key the host does not trust is not an update, even though
// the signature is internally valid — it is the wrong signer.
func TestVerifyRejectsAnUntrustedSigner(t *testing.T) {
	trusted, _ := testKey(t)
	attackerPublic, attackerPrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	release := signedRelease(attackerPrivate, "v1.2.3", []byte("a boat binary"))
	if err := Verify(release, trusted); !errors.Is(err, ErrSignature) {
		t.Fatalf("a release signed by an untrusted key returned %v, want ErrSignature", err)
	}
	// Sanity: the same release DOES verify under the attacker's own key, proving the
	// rejection above was about trust, not a broken signature.
	if err := Verify(release, attackerPublic); err != nil {
		t.Fatalf("attacker's release should verify under the attacker's own key: %v", err)
	}
}

// The signature binds the version: lifting a valid signature onto a different
// version number must fail, or a rollback could be forced by relabelling.
func TestVerifyRejectsAVersionSwap(t *testing.T) {
	public, private := testKey(t)
	release := signedRelease(private, "v1.2.3", []byte("a boat binary"))
	release.Manifest.Version = "v9.9.9" // same bytes, same signature, new label
	if err := Verify(release, public); !errors.Is(err, ErrSignature) {
		t.Fatalf("a version swap returned %v, want ErrSignature", err)
	}
}

func TestVerifyRejectsAMalformedManifest(t *testing.T) {
	public, private := testKey(t)
	for name, mutate := range map[string]func(*Release){
		"empty version":   func(r *Release) { r.Manifest.Version = "" },
		"short checksum":  func(r *Release) { r.Manifest.SHA256 = "abcd" },
		"non-hex":         func(r *Release) { r.Manifest.SHA256 = "zz" + r.Manifest.SHA256[2:] },
		"newline smuggle": func(r *Release) { r.Manifest.Version = "v1\nsha256=deadbeef" },
	} {
		release := signedRelease(private, "v1.2.3", []byte("a boat binary"))
		mutate(&release)
		if err := Verify(release, public); !errors.Is(err, ErrManifest) {
			t.Errorf("%s: returned %v, want ErrManifest", name, err)
		}
	}
}

func TestShouldApply(t *testing.T) {
	for _, testCase := range []struct {
		running, desired string
		want             bool
	}{
		{"6961bac", "6961bac", false}, // already there
		{"6961bac", "3bd1319", true},  // differs — update (upgrade or rollback)
		{"6961bac", "", false},        // Atlas asserted nothing
		{"", "6961bac", true},         // unknown running, desired asserted
	} {
		if got := ShouldApply(testCase.running, testCase.desired); got != testCase.want {
			t.Errorf("ShouldApply(%q,%q)=%v want %v", testCase.running, testCase.desired, got, testCase.want)
		}
	}
}
