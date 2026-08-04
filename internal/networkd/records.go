// Package networkd is the pure core of Atlas's ANCP network-control-plane daemon,
// re-ported from scripts/lib/atlas/networkd/. It is the application layer that
// survives the WO-5 cutover to hashicorp/memberlist: the wg peer/ownership record
// model, ed25519 record signing, the wg-mesh.conf render, conflict resolution, the
// key material on disk, and the in-memory applied state. The gossip substrate
// (transport, SWIM, anti-entropy) is memberlist's; the daemon that wires this core
// onto it (delegate.go, events.go, daemon.go) is a separate, later piece.
//
// Everything here is a pure function or an in-memory type — no goroutines, no
// gossip, and the only host touch is keys.go shelling `wg genkey`/`wg pubkey`
// through the run seam. That division is scripts/lib/atlas/networkd/'s, kept for
// the same reason: a test needs no host, no wg, and no root.
//
// records.go is the crown jewel — the data model + the generation semantics + the
// injection guard, ported EXACTLY from records.py (spec §7). Two record kinds:
//
//   - MembershipRecord — one per compute host, origin == host_id, mutated by the
//     origin only. The three render-interpolated fields (wg_public_key, endpoint,
//     mesh_address) are the injection surface Validate guards.
//   - OwnershipAdvertisement — a per-origin FULL SET of the /128s the origin owns
//     at a generation, never a delta. The effective table is the union of the
//     latest advertisement per origin; a /128 claimed by two origins is a conflict,
//     dropped and NEVER elected. Cross-origin generations are NEVER compared.
package networkd

import (
	"fmt"
	"sort"
	"unicode"
)

// InjectionError is the loud, typed refusal Validate returns for a record whose
// render-interpolated field carries whitespace or a control char — the config-file
// injection guard. It names the origin, the field and the offending value so an
// operator sees exactly which host tried to smuggle a [Peer] directive.
type InjectionError struct {
	HostID HostID
	Field  string
	Value  string
}

func (injection *InjectionError) Error() string {
	return fmt.Sprintf(
		"MembershipRecord from %s carries whitespace or control chars in %q: %q — "+
			"would inject directives into wg-mesh.conf at render",
		injection.HostID, injection.Field, injection.Value,
	)
}

// These three are semantic aliases, not distinct types, matching records.py's
// `HostID = str` / `IP6 = str` / `Generation = int`. The monotonicity invariant on
// Generation is enforced at apply time (OwnershipReplaces / MembershipReplaces),
// not by the type. Generation is unsigned because it is a 64-bit monotonic counter
// (spec §7.1) that a signed type would let wrap into negative territory.
type (
	HostID     = string
	IP6        = string
	Generation = uint64
)

// MembershipKind is the origin's declared intent (spec §14.4): a normal member, or
// a host shutting down.
type MembershipKind string

const (
	MembershipKindMember  MembershipKind = "member"
	MembershipKindLeaving MembershipKind = "leaving"
)

// MemberState is the origin's asserted state on the wire (spec §14.1). The
// observer-local suspicion ladder (alive→suspect→dead) is recomputed locally and
// never carried here: the wire state is the origin's own claim — normally alive,
// never suspect (only an observer suspects), and dead is never carried because dead
// records are GC'd.
type MemberState string

const (
	MemberStateAlive   MemberState = "alive"
	MemberStateLeaving MemberState = "leaving"
)

// MembershipRecord is one compute host as gossiped (spec §7.1). Origin == HostID.
//
// Endpoint is the bare public IPv6 (no port); render wraps it as [{endpoint}]:{port}.
// WireGuardPublicKey is base64 standard (32 raw Curve25519 bytes). MeshAddress is
// the infra /48 bus address fdaa:0:0:<idx>::1. SigningPublicKey (base64 ed25519, 32
// raw bytes) rides the record so a verifier can look up the right key for the
// signature; empty means "no signature required" (the pre-signing development path).
type MembershipRecord struct {
	HostID             HostID
	Kind               MembershipKind
	State              MemberState
	Endpoint           string
	WireGuardPublicKey string
	MeshAddress        IP6
	Generation         Generation
	SigningPublicKey   string
}

// Origin is always HostID (spec §19.2) — a MembershipRecord is only ever mutated by
// the host it describes.
func (record MembershipRecord) Origin() HostID { return record.HostID }

// Validate rejects records whose wire fields could inject config-file directives
// downstream, because render.go interpolates them verbatim into wg-mesh.conf.
//
// This is a security boundary, ported faithfully from records.py. The §19 auth
// layers authenticate WHO sent a record, not WHAT field values the author chose: a
// compromised-but-authenticated host can sign a MembershipRecord carrying
// wg_public_key = "validkey\n[Peer]\nPublicKey = <evil>\nAllowedIPs = fdab::/8", and
// the per-record signature still verifies (the attacker holds the private mate of
// their own signing key). A newline injects a rogue [Peer] into every host's
// wg-mesh.conf via render; `wg-quick strip` preserves all [Peer] sections, so the
// injected peer reaches the kernel with an attacker-controlled pubkey. Spec §19.2
// bounds a compromised host to its own peer slot; injection escapes that bound, so
// anything outside the no-whitespace, no-control-chars class is refused for the
// three interpolated fields.
//
// The loose check (whitespace + control chars only) is sufficient because any
// arbitrary-peer injection needs a line separator in one of the three fields;
// non-whitespace chars that merely fat-finger the line (a `]` inside endpoint) are
// fail-closed by `wg syncconf`'s strict parser rejecting the whole config, not an
// injection of an extra [Peer] slot. Called at every parse boundary and again from
// render as belt-and-suspenders, so a directly-constructed record that bypassed
// parse still cannot reach the config body.
func (record MembershipRecord) Validate() error {
	for _, field := range []struct{ name, value string }{
		{"wg_public_key", record.WireGuardPublicKey},
		{"endpoint", record.Endpoint},
		{"mesh_address", record.MeshAddress},
	} {
		for _, character := range field.value {
			if unicode.IsSpace(character) || character < 32 {
				return &InjectionError{HostID: record.HostID, Field: field.name, Value: field.value}
			}
		}
	}
	return nil
}

// OwnershipAdvertisement is a per-origin FULL SET of owned /128s at a Generation
// (spec §7.2). Origin == owner_host always (§19.2); a relay forwards it but only the
// origin publishes it. Never a delta: removing a /128 is a later advertisement with
// a smaller set at a higher generation.
//
// Owned is kept sorted and de-duplicated (see OwningAdvertisement) so two
// advertisements of the same set render and sign to the same bytes, which the
// duplicate-suppression cache (spec §13.3) relies on. Signature rides the wire but
// is NOT part of the record's identity — dedupe keys on (origin, generation) only.
type OwnershipAdvertisement struct {
	Origin     HostID
	Generation Generation
	Owned      []IP6
	Signature  string
}

// Owns reports whether this advertisement claims ip. Owned is sorted, so this is a
// binary search — the membership test conflict detection (conflicts.go) runs per
// /128 per apply.
func (advertisement OwnershipAdvertisement) Owns(ip IP6) bool {
	index := sort.SearchStrings(advertisement.Owned, ip)
	return index < len(advertisement.Owned) && advertisement.Owned[index] == ip
}

// OwningAdvertisement builds an advertisement, sorting and de-duplicating owned so
// equality is order-insensitive (the §13.3 cache + the §16.2 render both depend on
// this canonical form).
func OwningAdvertisement(origin HostID, generation Generation, owned []IP6) OwnershipAdvertisement {
	return OwnershipAdvertisement{Origin: origin, Generation: generation, Owned: sortedUnique(owned)}
}

// OwnershipTable is the effective ownership table (spec §7.2), derived and never
// stored. OwnerOf[ip] is the unique HostID whose latest advertisement claims ip,
// populated only for a /128 owned by exactly one origin. Conflicts holds every /128
// claimed by two or more origins; such a /128 is NOT in OwnerOf — spec §7.3 drops
// and reports it, never elects a winner.
type OwnershipTable struct {
	OwnerOf   map[IP6]HostID
	Conflicts map[IP6]struct{}
}

// IsConflict reports whether ip is double-owned in this table.
func (table OwnershipTable) IsConflict(ip IP6) bool {
	_, conflicted := table.Conflicts[ip]
	return conflicted
}

// EffectiveOwnership computes the effective ownership table as the union of the
// latest advertisement per origin (spec §7.2). A /128 in two-or-more origins' active
// sets is a conflict (§7.3): it lands in Conflicts, NOT in OwnerOf. Generations are
// NOT compared across origins (Issue C) — they only compete within an origin, which
// the caller's apply rule (§13.2) already enforced before storing into
// latestPerOrigin.
func EffectiveOwnership(latestPerOrigin map[HostID]OwnershipAdvertisement) OwnershipTable {
	claimants := map[IP6][]HostID{}
	for origin, advertisement := range latestPerOrigin {
		for _, ip := range advertisement.Owned {
			claimants[ip] = append(claimants[ip], origin)
		}
	}
	ownerOf := map[IP6]HostID{}
	conflicts := map[IP6]struct{}{}
	for ip, origins := range claimants {
		// ≥ 2 DISTINCT origins is the conflict. The distinct-count guard is
		// defensive: an origin should never appear twice for one ip, but the drop
		// rule must hold regardless, so a single origin listed twice still elects.
		if distinctCount(origins) > 1 {
			conflicts[ip] = struct{}{}
		} else {
			ownerOf[ip] = origins[0]
		}
	}
	return OwnershipTable{OwnerOf: ownerOf, Conflicts: conflicts}
}

// MembershipReplaces is the §10.3 / §13.2 apply rule for a Membership Record: an
// incoming record replaces the existing one iff its Generation is strictly higher
// (same origin by §19.2; cross-origin forwarding is rejected upstream). Equal
// generation is a no-op (idempotent re-delivery); lower is a stale replay to drop.
func MembershipReplaces(existing *MembershipRecord, incoming MembershipRecord) bool {
	return existing == nil || incoming.Generation > existing.Generation
}

// OwnershipReplaces is the §13.2 apply rule for an Ownership Advertisement — the
// same per-origin monotonic rule. The full-set model makes an equal-generation
// re-delivery byte-equal, so dropping it on equality is also correct; strict `>`
// matches the membership rule and lets the §13.3 dedupe cache catch redelivery.
func OwnershipReplaces(existing *OwnershipAdvertisement, incoming OwnershipAdvertisement) bool {
	return existing == nil || incoming.Generation > existing.Generation
}

// DedupeKey is the §13.3 duplicate-suppression key: (origin, kind, generation).
// Membership and Ownership live in disjoint kind namespaces, so a membership and an
// ownership record from the same origin at the same generation never collide.
type DedupeKey struct {
	Origin     HostID
	Kind       string
	Generation Generation
}

// MembershipDedupeKey is the (origin, "membership", generation) key.
func MembershipDedupeKey(record MembershipRecord) DedupeKey {
	return DedupeKey{Origin: record.HostID, Kind: "membership", Generation: record.Generation}
}

// OwnershipDedupeKey is the (origin, "ownership", generation) key.
func OwnershipDedupeKey(advertisement OwnershipAdvertisement) DedupeKey {
	return DedupeKey{Origin: advertisement.Origin, Kind: "ownership", Generation: advertisement.Generation}
}

// sortedUnique returns values sorted with duplicates removed, without mutating the
// input — the canonical form the owned set is always stored in.
func sortedUnique(values []IP6) []IP6 {
	if len(values) == 0 {
		return nil
	}
	sorted := append([]IP6(nil), values...)
	sort.Strings(sorted)
	unique := sorted[:1]
	for _, value := range sorted[1:] {
		if value != unique[len(unique)-1] {
			unique = append(unique, value)
		}
	}
	return unique
}

// distinctCount is the number of distinct HostIDs in origins — cheap because the
// per-/128 claimant list is tiny (bounded by the fleet size that owns one address).
func distinctCount(origins []HostID) int {
	seen := map[HostID]struct{}{}
	for _, origin := range origins {
		seen[origin] = struct{}{}
	}
	return len(seen)
}
