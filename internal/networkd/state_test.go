package networkd

import "testing"

// The monotonic apply rule holds through the AppliedState: a higher generation
// changes the table, an equal or lower one does not, and every record is marked seen
// so a byte-equal re-delivery is short-circuited.
func TestApplyMembershipIsMonotonicAndMarksSeen(t *testing.T) {
	state := NewAppliedState(0)
	first := MembershipRecord{HostID: "host-a", Generation: 1}
	if !state.ApplyMembership(first, nil) {
		t.Fatal("the first record for an origin must change the table")
	}
	if state.ApplyMembership(first, nil) {
		t.Fatal("a re-delivered same-generation record must be a no-op apply")
	}
	if !state.SeenAlready(MembershipDedupeKey(first)) {
		t.Fatal("an applied record must be recorded in the dedupe cache")
	}
	if !state.ApplyMembership(MembershipRecord{HostID: "host-a", Generation: 2}, nil) {
		t.Fatal("a strictly higher generation must change the table")
	}
	if state.ApplyMembership(MembershipRecord{HostID: "host-a", Generation: 1}, nil) {
		t.Fatal("a lower generation (stale replay) must not change the table")
	}
}

// A signed record's pubkey flows into the verifier cache so a later record from the
// same origin can be checked — the key-rotation path.
func TestApplyMembershipUpdatesPubkeyCache(t *testing.T) {
	state := NewAppliedState(0)
	cache := map[HostID]string{}
	state.ApplyMembership(MembershipRecord{HostID: "host-a", Generation: 1, SigningPublicKey: "key-one"}, cache)
	if cache["host-a"] != "key-one" {
		t.Fatalf("pubkey cache = %v, want key-one", cache)
	}
}

func TestApplyOwnershipIsMonotonic(t *testing.T) {
	state := NewAppliedState(0)
	if !state.ApplyOwnership(OwningAdvertisement("host-a", 1, []IP6{"fdab::1"})) {
		t.Fatal("the first advertisement must change the table")
	}
	if state.ApplyOwnership(OwningAdvertisement("host-a", 1, []IP6{"fdab::2"})) {
		t.Fatal("an equal-generation advertisement must not change the table")
	}
	if !state.ApplyOwnership(OwningAdvertisement("host-a", 2, []IP6{"fdab::2"})) {
		t.Fatal("a higher-generation advertisement must change the table")
	}
}

func TestBumpOwnGenerationIncrements(t *testing.T) {
	state := NewAppliedState(0)
	if state.BumpOwnGeneration() != 1 || state.BumpOwnGeneration() != 2 {
		t.Fatal("own generation must increment monotonically from zero")
	}
	if state.OwnGeneration() != 2 {
		t.Fatalf("own generation = %d, want 2", state.OwnGeneration())
	}
}

// The dedupe cache is a bounded LRU: the oldest key is evicted at capacity, and a hit
// refreshes recency so a still-arriving duplicate survives.
func TestSeenCacheEvictsOldestAndRefreshesOnHit(t *testing.T) {
	cache := newSeenCache(2)
	first := DedupeKey{Origin: "a", Kind: "ownership", Generation: 1}
	second := DedupeKey{Origin: "b", Kind: "ownership", Generation: 1}
	third := DedupeKey{Origin: "c", Kind: "ownership", Generation: 1}

	cache.mark(first)
	cache.mark(second)
	if !cache.seen(first) { // refresh first, so second becomes the eviction candidate
		t.Fatal("first should still be cached before eviction")
	}
	cache.mark(third)
	if cache.seen(second) {
		t.Fatal("second should have been evicted as the least recently used")
	}
	if !cache.seen(first) || !cache.seen(third) {
		t.Fatal("the refreshed key and the newest key should both survive")
	}
}

// The §14.3 retention: a dead host's record moves to the render-only view, keeps
// rendering during ownership_grace, then is reaped once the window elapses.
func TestRoutableDeadRetentionRespectsGraceWindow(t *testing.T) {
	state := NewAppliedState(0)
	dead := MembershipRecord{HostID: "host-a", Generation: 1, WireGuardPublicKey: publicKeyA, MeshAddress: "fdaa:0:0:a::1"}
	state.ApplyMembership(dead, nil)
	state.ApplyOwnership(OwningAdvertisement("host-a", 1, []IP6{"fdab::a1"}))

	if !state.MarkRoutableDead("host-a") {
		t.Fatal("a live record must move into the render-only view")
	}
	if _, live := state.Membership()["host-a"]; live {
		t.Fatal("a reaped host must be gone from the gossip/probe view")
	}
	if _, rendered := state.RenderMembers()["host-a"]; !rendered {
		t.Fatal("a routable-dead host must still render its [Peer] during the grace window")
	}

	if state.GCOriginIfDead("host-a", 0, 60, 30) {
		t.Fatal("ownership must survive inside the grace window (30 < 60)")
	}
	if !state.GCOriginIfDead("host-a", 0, 60, 70) {
		t.Fatal("ownership must be reaped once the grace window elapses (70 >= 60)")
	}
	if _, rendered := state.RenderMembers()["host-a"]; rendered {
		t.Fatal("once ownership is reaped the [Peer] must disappear too")
	}
}

// A late-refuting host coming back alive clears its stale routable-dead record so the
// fresh record is not carried alongside the dead one.
func TestApplyMembershipClearsRoutableDeadOnRefute(t *testing.T) {
	state := NewAppliedState(0)
	state.ApplyMembership(MembershipRecord{HostID: "host-a", Generation: 1}, nil)
	state.MarkRoutableDead("host-a")

	state.ApplyMembership(MembershipRecord{HostID: "host-a", Generation: 2}, nil)
	if _, live := state.Membership()["host-a"]; !live {
		t.Fatal("a refuting host must return to the live view")
	}
	// RenderMembers must carry exactly the live record now, not a duplicate.
	if got := state.RenderMembers()["host-a"].Generation; got != 2 {
		t.Fatalf("render record generation = %d, want the live 2", got)
	}
}

// The live record wins over a stale render-only one for the same host.
func TestRenderMembersPrefersLiveOverRoutableDead(t *testing.T) {
	state := NewAppliedState(0)
	state.ApplyMembership(MembershipRecord{HostID: "host-a", Generation: 5}, nil)
	// Seed a stale record straight into the render-only view for the same host.
	state.routableDead["host-a"] = MembershipRecord{HostID: "host-a", Generation: 1}
	if got := state.RenderMembers()["host-a"].Generation; got != 5 {
		t.Fatalf("render must prefer the live record, got generation %d", got)
	}
}
