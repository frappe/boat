package networkd

import (
	"strings"
	"testing"
)

// Three peer public keys ordered A < B < Z, so the render's sort-by-pubkey is
// deterministic and self (Z) sorts last where the skip is visible.
const (
	publicKeyA    = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	publicKeyB    = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB="
	publicKeySelf = "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ="
)

func member(hostID, endpoint, publicKey, mesh string) MembershipRecord {
	return MembershipRecord{
		HostID: hostID, Kind: MembershipKindMember, State: MemberStateAlive,
		Endpoint: endpoint, WireGuardPublicKey: publicKey, MeshAddress: mesh,
	}
}

// The canonical wg-mesh.conf body: [Interface] then one [Peer] per OTHER member,
// peers sorted by pubkey, AllowedIPs = owned /128s ∪ the peer's mesh /128, sorted.
func TestRenderProducesCanonicalConfig(t *testing.T) {
	members := map[HostID]MembershipRecord{
		"host-a":    member("host-a", "2001:db8::a", publicKeyA, "fdaa:0:0:a::1"),
		"host-b":    member("host-b", "2001:db8::b", publicKeyB, "fdaa:0:0:b::1"),
		"host-self": member("host-self", "2001:db8::s", publicKeySelf, "fdaa:0:0:s::1"),
	}
	table := EffectiveOwnership(map[HostID]OwnershipAdvertisement{
		"host-a": OwningAdvertisement("host-a", 1, []IP6{"fdab::a1"}),
		"host-b": OwningAdvertisement("host-b", 1, []IP6{"fdab::b1"}),
	})

	body, err := RenderWireGuardDesired("host-self", members, table)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := "[Interface]\n" +
		"ListenPort = 51820\n" +
		"\n" +
		"[Peer]\n" +
		"PublicKey = " + publicKeyA + "\n" +
		"AllowedIPs = fdaa:0:0:a::1/128, fdab::a1/128\n" +
		"Endpoint = [2001:db8::a]:51820\n" +
		"PersistentKeepalive = 25\n" +
		"\n" +
		"[Peer]\n" +
		"PublicKey = " + publicKeyB + "\n" +
		"AllowedIPs = fdaa:0:0:b::1/128, fdab::b1/128\n" +
		"Endpoint = [2001:db8::b]:51820\n" +
		"PersistentKeepalive = 25\n" +
		"\n"
	if body != want {
		t.Fatalf("render mismatch:\n got:\n%q\nwant:\n%q", body, want)
	}
}

// A host does not peer with itself, and a member whose wg_public_key has not yet
// propagated is skipped rather than emitted with an empty key.
func TestRenderSkipsSelfAndKeylessPeers(t *testing.T) {
	members := map[HostID]MembershipRecord{
		"host-self": member("host-self", "2001:db8::s", publicKeySelf, "fdaa:0:0:s::1"),
		"host-a":    member("host-a", "2001:db8::a", publicKeyA, "fdaa:0:0:a::1"),
		"host-c":    member("host-c", "2001:db8::c", "", "fdaa:0:0:c::1"),
	}
	table := EffectiveOwnership(map[HostID]OwnershipAdvertisement{
		"host-c": OwningAdvertisement("host-c", 1, []IP6{"fdab::c1"}),
	})
	body, err := RenderWireGuardDesired("host-self", members, table)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, publicKeySelf) {
		t.Error("self must not be rendered as a [Peer]")
	}
	if strings.Contains(body, "host-c") || strings.Contains(body, "fdab::c1") {
		t.Error("a keyless peer, and the /128s it owns, must not be rendered")
	}
}

// A double-owned /128 (in ownership.Conflicts) is dropped from routing — it never
// appears under any peer.
func TestRenderDropsConflictingOwnedAddress(t *testing.T) {
	members := map[HostID]MembershipRecord{
		"host-a": member("host-a", "2001:db8::a", publicKeyA, "fdaa:0:0:a::1"),
		"host-b": member("host-b", "2001:db8::b", publicKeyB, "fdaa:0:0:b::1"),
	}
	table := EffectiveOwnership(map[HostID]OwnershipAdvertisement{
		"host-a": OwningAdvertisement("host-a", 1, []IP6{"fdab::shared"}),
		"host-b": OwningAdvertisement("host-b", 1, []IP6{"fdab::shared"}),
	})
	body, err := RenderWireGuardDesired("host-self", members, table)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "fdab::shared") {
		t.Fatalf("a conflicting /128 must be dropped from every peer, got:\n%s", body)
	}
}

// A mesh_address collision (two peers claiming the same infra /128) is dropped from
// BOTH peers and reported — the H2 fold that stops WireGuard cryptokey misdelivery.
func TestRenderFoldsMeshAddressCollision(t *testing.T) {
	members := map[HostID]MembershipRecord{
		"host-a": member("host-a", "2001:db8::a", publicKeyA, "fdaa:0:0:x::1"),
		"host-b": member("host-b", "2001:db8::b", publicKeyB, "fdaa:0:0:x::1"),
	}
	body, conflicts, err := RenderWireGuardDesiredWithConflicts("host-self", members, OwnershipTable{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "fdaa:0:0:x::1") {
		t.Fatalf("a colliding mesh_address must be dropped from both peers, got:\n%s", body)
	}
	origins, reported := conflicts["fdaa:0:0:x::1"]
	if !reported {
		t.Fatal("the mesh_address collision must be reported in the render conflict map")
	}
	if len(origins) != 2 {
		t.Fatalf("both contending peers must be reported, got %v", origins)
	}
}

// A record that reaches render carrying an injection is refused at the doorstep —
// belt-and-suspenders on top of the parse-boundary check.
func TestRenderRefusesInjectedRecord(t *testing.T) {
	members := map[HostID]MembershipRecord{
		"host-a": member("host-a", "2001:db8::1\n[Peer]", publicKeyA, "fdaa:0:0:a::1"),
	}
	if _, err := RenderWireGuardDesired("host-self", members, OwnershipTable{}); err == nil {
		t.Fatal("render must refuse a record with a newline in an interpolated field")
	}
}

// The §16.3 invariant, exercised directly: no /128 may sit under two peers, and a
// conflict must never leak into owner_of.
func TestAssertNoInputOverlapCatchesDuplicatesAndLeaks(t *testing.T) {
	overlapping := map[HostID]map[IP6]struct{}{
		"host-a": {"fdab::1/128": {}},
		"host-b": {"fdab::1/128": {}},
	}
	if err := assertNoInputOverlap(overlapping, OwnershipTable{}, nil); err == nil {
		t.Error("a /128 under two peers must be caught")
	}
	leakedTable := OwnershipTable{
		OwnerOf:   map[IP6]HostID{"fdab::x": "host-a"},
		Conflicts: map[IP6]struct{}{"fdab::x": {}},
	}
	if err := assertNoInputOverlap(nil, leakedTable, nil); err == nil {
		t.Error("a conflict that also sits in owner_of must be caught")
	}
}
