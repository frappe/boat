package networkd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeSeedFiles writes a seed.json (and, when sign is true, an operator-signed
// seed.json.sig over its exact bytes) into a temp dir, returning the seed path and the
// operator keypair.
func writeSeedFiles(t *testing.T, entries []SeedEntry, sign bool) (string, string, string) {
	t.Helper()
	operatorPrivate, operatorPublic, err := GenerateSigningKeypair()
	if err != nil {
		t.Fatalf("GenerateSigningKeypair: %v", err)
	}
	directory := t.TempDir()
	seedPath := filepath.Join(directory, "seed.json")
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(seedPath, raw, 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	if sign {
		signature, err := SignDetached(raw, operatorPrivate)
		if err != nil {
			t.Fatalf("SignDetached: %v", err)
		}
		if err := os.WriteFile(seedPath+seedSigSuffix, []byte(signature), 0o644); err != nil {
			t.Fatalf("write sig: %v", err)
		}
	}
	return seedPath, operatorPrivate, operatorPublic
}

func sampleEntries() []SeedEntry {
	return []SeedEntry{
		{HostID: "host-a", Endpoint: "2001:db8::a", WireGuardPublicKey: testWireGuardPublic, SigningPublicKey: "keyA", MeshAddress: "fdaa:0:0:a::1", Generation: 1},
		{HostID: "host-b", Endpoint: "2001:db8::b", WireGuardPublicKey: testWireGuardPrivate, SigningPublicKey: "keyB", MeshAddress: "fdaa:0:0:b::1", Generation: 1},
	}
}

// A well-formed, operator-signed seed loads, and yields the trust directory + the
// bracketed IPv6 join addresses.
func TestLoadSeedVerifiesAndDerives(t *testing.T) {
	seedPath, _, operatorPublic := writeSeedFiles(t, sampleEntries(), true)
	seed, err := LoadSeed(seedPath, operatorPublic)
	if err != nil {
		t.Fatalf("LoadSeed: %v", err)
	}
	if len(seed.Entries) != 2 {
		t.Fatalf("loaded %d entries, want 2", len(seed.Entries))
	}
	trust := seed.TrustDirectory()
	if trust["host-a"] != "keyA" || trust["host-b"] != "keyB" {
		t.Fatalf("trust directory = %v", trust)
	}
	addresses := seed.JoinAddresses(7946)
	if len(addresses) != 2 || addresses[0] != "[2001:db8::a]:7946" {
		t.Fatalf("join addresses = %v, want bracketed IPv6 host:port", addresses)
	}
}

// Fail-closed: an operator key is configured but the .sig is absent — refuse to
// install an unsigned trust root.
func TestLoadSeedFailsClosedWhenSignatureMissing(t *testing.T) {
	seedPath, _, operatorPublic := writeSeedFiles(t, sampleEntries(), false)
	if _, err := LoadSeed(seedPath, operatorPublic); err == nil {
		t.Fatal("a configured operator key with no seed signature must fail closed")
	}
}

// A tampered seed (bytes changed after signing) fails verification.
func TestLoadSeedRejectsTamperedBytes(t *testing.T) {
	seedPath, _, operatorPublic := writeSeedFiles(t, sampleEntries(), true)
	if err := os.WriteFile(seedPath, []byte(`[{"host_id":"evil","endpoint":"2001:db8::e","mesh_address":"fdaa:0:0:e::1"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSeed(seedPath, operatorPublic); err == nil {
		t.Fatal("a seed whose bytes changed after signing must fail verification")
	}
}

// The dev/test posture: no operator key configured loads UNVERIFIED (bring-up is not
// blocked), which is the only path that tolerates a missing signature.
func TestLoadSeedLoadsUnverifiedWithoutOperatorKey(t *testing.T) {
	seedPath, _, _ := writeSeedFiles(t, sampleEntries(), false)
	seed, err := LoadSeed(seedPath, "")
	if err != nil {
		t.Fatalf("no-operator-key seed load must succeed unverified: %v", err)
	}
	if len(seed.Entries) != 2 {
		t.Fatalf("loaded %d entries, want 2", len(seed.Entries))
	}
}

// The injection guard runs at the seed doorstep: a newline in an interpolated field is
// refused before it can smuggle a [Peer] directive into a render.
func TestLoadSeedRejectsInjectionInEntry(t *testing.T) {
	entries := []SeedEntry{
		{HostID: "host-a", Endpoint: "2001:db8::a", WireGuardPublicKey: "good\n[Peer]", SigningPublicKey: "keyA", MeshAddress: "fdaa:0:0:a::1", Generation: 1},
	}
	seedPath, _, _ := writeSeedFiles(t, entries, false)
	if _, err := LoadSeed(seedPath, ""); err == nil {
		t.Fatal("a seed entry with a newline in wg_public_key must be refused")
	}
}

// A missing seed is an error for LoadSeed but the empty, no-error come-up-peer-empty
// posture for LoadSeedOptional.
func TestLoadSeedOptionalToleratesMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.json")
	if _, err := LoadSeed(missing, ""); err == nil {
		t.Fatal("LoadSeed must error on a missing file")
	}
	seed, err := LoadSeedOptional(missing, "")
	if err != nil || len(seed.Entries) != 0 {
		t.Fatalf("LoadSeedOptional on a missing file = (%v, %v), want empty + nil", seed, err)
	}
}
