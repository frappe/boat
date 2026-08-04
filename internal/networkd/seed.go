// Bootstrap seed loader, ported from seed.py (spec §8 / §9.2 / §19.4).
//
// The seed file is the sole out-of-band trust root at first boot: an operator-signed
// list of currently-known hosts the controller writes to /etc/atlas-networkd/seed.json
// at provision, alongside seed.json.sig (a detached ed25519 signature over the file's
// EXACT bytes) and operator-public-key (the §19.5 newcomer-introduction trust root).
//
// LoadSeed verifies that operator signature before returning any entry (spec §9.2 —
// the seed is the sole trust root, so a bad or missing signature is a HARD failure
// when an operator key is configured: no partial bring-up, no "trust the unsigned
// list" fallback). A cluster with NO operator key (the dev/test posture) loads
// unverified with a loud warning, so bring-up is not blocked while production stays
// fail-closed. From the seed the daemon derives three things: the host_id→signing-
// pubkey trust directory (§19.4), the []string join addresses memberlist dials, and
// the membership pre-population records.
package networkd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"strings"
)

const (
	// DefaultSeedPath and its siblings are the fixed bootstrap layout the controller
	// writes and the systemd unit names literally.
	DefaultSeedPath            = "/etc/atlas-networkd/seed.json"
	DefaultOperatorPubkeyPath  = "/etc/atlas-networkd/operator-public-key"
	DefaultIntroductionSigPath = "/etc/atlas-networkd/introduction-signature"

	// seedSigSuffix names the detached operator signature that sits beside the seed.
	seedSigSuffix = ".sig"
)

// SeedEntry is one known host as the operator listed it. Mirrors the seed.json shape
// byte-for-byte (the field names are a cross-impl contract — the controller writes
// them, spec §8.3).
type SeedEntry struct {
	HostID             HostID     `json:"host_id"`
	Endpoint           string     `json:"endpoint"`
	WireGuardPublicKey string     `json:"wg_public_key"`
	SigningPublicKey   string     `json:"signing_public_key"`
	MeshAddress        IP6        `json:"mesh_address"`
	Generation         Generation `json:"generation"`
}

// Record turns a seed entry into a MembershipRecord marked alive/member at the
// generation the seed carried, for the initial membership pre-population (spec §8).
func (entry SeedEntry) Record() MembershipRecord {
	generation := entry.Generation
	if generation == 0 {
		generation = 1
	}
	return MembershipRecord{
		HostID:             entry.HostID,
		Kind:               MembershipKindMember,
		State:              MemberStateAlive,
		Endpoint:           entry.Endpoint,
		WireGuardPublicKey: entry.WireGuardPublicKey,
		MeshAddress:        entry.MeshAddress,
		Generation:         generation,
		SigningPublicKey:   entry.SigningPublicKey,
	}
}

// Seed is the loaded, verified set of known hosts.
type Seed struct {
	Entries []SeedEntry
}

// LoadSeed reads and verifies the seed file. A missing file is an error — a fresh
// host with no seed cannot join, and a silent peer-empty bring-up would mask a
// misconfigured provision (use LoadSeedOptional for the deliberate lone-host
// posture). The operator signature over the seed's exact bytes is verified BEFORE
// any entry is returned when operatorPublicKey is non-empty (fail closed); an empty
// operator key loads unverified with a warning (dev/test only). Every entry's
// render-interpolated fields are injection-validated at the doorstep.
func LoadSeed(seedPath, operatorPublicKey string) (Seed, error) {
	raw, err := os.ReadFile(seedPath)
	if err != nil {
		return Seed{}, fmt.Errorf("seed file at %s: %w", seedPath, err)
	}
	if err := verifySeedSignature(seedPath, raw, operatorPublicKey); err != nil {
		return Seed{}, err
	}
	var entries []SeedEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return Seed{}, fmt.Errorf("seed file at %s is not a JSON list of host entries: %w", seedPath, err)
	}
	for _, entry := range entries {
		if err := entry.Record().Validate(); err != nil {
			return Seed{}, fmt.Errorf("seed file at %s: %w", seedPath, err)
		}
	}
	return Seed{Entries: entries}, nil
}

// LoadSeedOptional is LoadSeed but returns an empty seed when the file is absent —
// the deliberate come-up-peer-empty-and-wait posture (spec §9.2). A present-but-bad
// or unverifiable file still fails loud.
func LoadSeedOptional(seedPath, operatorPublicKey string) (Seed, error) {
	if _, err := os.Stat(seedPath); os.IsNotExist(err) {
		return Seed{}, nil
	}
	return LoadSeed(seedPath, operatorPublicKey)
}

// verifySeedSignature checks the operator's detached signature over the seed's exact
// bytes (spec §9.2 / §19.4). Fail-closed when an operator key is configured (a
// missing or invalid .sig raises and installs nothing); a warn-and-continue dev
// posture when none is.
func verifySeedSignature(seedPath string, raw []byte, operatorPublicKey string) error {
	if operatorPublicKey == "" {
		slog.Warn(
			"atlas-networkd: seed loaded UNVERIFIED — no operator public key is configured, so the "+
				"trust root cannot be checked. Provision an operator keypair for a production cluster "+
				"(spec §9.2 / §19.4).",
			"seed_path", seedPath,
		)
		return nil
	}
	sigPath := seedPath + seedSigSuffix
	sigData, err := os.ReadFile(sigPath)
	if err != nil {
		return fmt.Errorf(
			"%w: seed %s has no operator signature at %s, but an operator public key is configured — "+
				"refusing to install an unsigned trust root (spec §9.2)", ErrSignature, seedPath, sigPath,
		)
	}
	return VerifyDetached(raw, strings.TrimSpace(string(sigData)), operatorPublicKey)
}

// TrustDirectory is the host_id→signing-pubkey directory the seed anchors (spec
// §19.4) — the initial trust dir the daemon verifies peer Meta and ownership sigs
// against. Entries with an empty signing key are skipped (a host bootstrapped before
// signing; its Meta will demand a §19.5 introduction cert on first contact).
func (seed Seed) TrustDirectory() map[HostID]string {
	directory := make(map[HostID]string, len(seed.Entries))
	for _, entry := range seed.Entries {
		if entry.SigningPublicKey != "" {
			directory[entry.HostID] = entry.SigningPublicKey
		}
	}
	return directory
}

// JoinAddresses is the memberlist join list: "[<endpoint>]:<port>" for every seed
// host (the bracket form AF_INET6 needs). Self is left in — memberlist tolerates a
// node dialing its own address — but an empty endpoint is dropped so a malformed
// entry does not become a bad dial target.
func (seed Seed) JoinAddresses(ancpPort int) []string {
	addresses := make([]string, 0, len(seed.Entries))
	for _, entry := range seed.Entries {
		if entry.Endpoint == "" {
			continue
		}
		addresses = append(addresses, fmt.Sprintf("[%s]:%d", entry.Endpoint, ancpPort))
	}
	return addresses
}

// LoadOperatorPublicKey reads the operator provision pubkey (base64 ed25519, spec
// §19.4 / §19.5). A missing or empty file returns "" — the dev/test posture, or a
// cluster not yet wired with operator signing. A present, non-empty file is returned
// verbatim; malformed bytes surface later at the one verify site rather than here.
func LoadOperatorPublicKey(path string) (string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("operator public key at %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// LoadOptionalLine reads a one-line file, returning "" when it is absent — used for
// the §19.5 introduction-signature, present only on a host that joined an existing
// cluster post-bootstrap.
func LoadOptionalLine(path string) (string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("file at %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// validMeshAddress reports whether address parses as an IPv6 address — the code-side
// check that lets the bring-up sudoers line pin `ip -6 addr replace <addr>/128 dev
// wg-mesh` to a validated value rather than an open wildcard.
func validMeshAddress(address string) bool {
	parsed, err := netip.ParseAddr(address)
	return err == nil && parsed.Is6()
}
