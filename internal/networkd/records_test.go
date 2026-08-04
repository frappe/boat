package networkd

import (
	"errors"
	"reflect"
	"testing"
)

// A well-formed record passes; a whitespace or control char in any of the three
// render-interpolated fields is refused — the injection guard.
func TestValidateRejectsInjectionInInterpolatedFields(t *testing.T) {
	good := MembershipRecord{
		HostID: "host-a", Kind: MembershipKindMember, State: MemberStateAlive,
		Endpoint: "2001:db8::1", WireGuardPublicKey: testWireGuardPublic, MeshAddress: "fdaa:0:0:a::1",
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("a clean record should validate, got %v", err)
	}
	for _, testCase := range []struct {
		name   string
		mutate func(*MembershipRecord)
	}{
		{"newline in wg_public_key", func(r *MembershipRecord) {
			r.WireGuardPublicKey = "validkey\n[Peer]\nPublicKey = evil"
		}},
		{"space in endpoint", func(r *MembershipRecord) { r.Endpoint = "2001:db8::1 evil" }},
		{"tab in mesh_address", func(r *MembershipRecord) { r.MeshAddress = "fdaa::1\tevil" }},
		{"control char in endpoint", func(r *MembershipRecord) { r.Endpoint = "2001:db8::1\x01" }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			record := good
			testCase.mutate(&record)
			err := record.Validate()
			if err == nil {
				t.Fatalf("expected %s to be rejected", testCase.name)
			}
			var injection *InjectionError
			if !errors.As(err, &injection) {
				t.Fatalf("expected an *InjectionError, got %T: %v", err, err)
			}
		})
	}
}

// Distinct /128s land in owner_of; a /128 in two origins' sets is a conflict, dropped
// from owner_of and never elected.
func TestEffectiveOwnershipConflictIsDroppedNeverElected(t *testing.T) {
	latest := map[HostID]OwnershipAdvertisement{
		"host-a": OwningAdvertisement("host-a", 3, []IP6{"fdab::1", "fdab::shared"}),
		"host-b": OwningAdvertisement("host-b", 9, []IP6{"fdab::2", "fdab::shared"}),
	}
	table := EffectiveOwnership(latest)
	if table.OwnerOf["fdab::1"] != "host-a" || table.OwnerOf["fdab::2"] != "host-b" {
		t.Fatalf("distinct /128s should map to their sole owner, got %v", table.OwnerOf)
	}
	if _, elected := table.OwnerOf["fdab::shared"]; elected {
		t.Fatalf("a double-owned /128 must never be elected into owner_of, got %v", table.OwnerOf)
	}
	if !table.IsConflict("fdab::shared") {
		t.Fatal("the double-owned /128 must be reported as a conflict")
	}
}

// The higher generation does NOT win a cross-origin contest — the whole point of
// Issue C. host-a at gen 100 and host-b at gen 1 both claiming a /128 is a conflict,
// not "host-a wins because 100 > 1".
func TestEffectiveOwnershipNeverComparesGenerationsAcrossOrigins(t *testing.T) {
	latest := map[HostID]OwnershipAdvertisement{
		"host-a": OwningAdvertisement("host-a", 100, []IP6{"fdab::x"}),
		"host-b": OwningAdvertisement("host-b", 1, []IP6{"fdab::x"}),
	}
	table := EffectiveOwnership(latest)
	if _, elected := table.OwnerOf["fdab::x"]; elected {
		t.Fatal("a cross-origin contest must not be resolved by generation")
	}
	if !table.IsConflict("fdab::x") {
		t.Fatal("a cross-origin double-claim is a conflict regardless of generation")
	}
}

// The per-origin monotonic apply rule: nil/first applies, strictly-higher applies,
// equal and lower do not.
func TestReplaceRulesAreStrictlyMonotonicPerOrigin(t *testing.T) {
	base := MembershipRecord{HostID: "host-a", Generation: 5}
	if !MembershipReplaces(nil, base) {
		t.Error("first record for an origin must apply")
	}
	if !MembershipReplaces(&base, MembershipRecord{HostID: "host-a", Generation: 6}) {
		t.Error("a strictly higher generation must replace")
	}
	if MembershipReplaces(&base, MembershipRecord{HostID: "host-a", Generation: 5}) {
		t.Error("an equal generation must not replace (idempotent re-delivery)")
	}
	if MembershipReplaces(&base, MembershipRecord{HostID: "host-a", Generation: 4}) {
		t.Error("a lower generation must not replace (stale replay)")
	}

	advertisement := OwningAdvertisement("host-a", 5, []IP6{"fdab::1"})
	if !OwnershipReplaces(nil, advertisement) {
		t.Error("first advertisement for an origin must apply")
	}
	if OwnershipReplaces(&advertisement, OwningAdvertisement("host-a", 5, []IP6{"fdab::2"})) {
		t.Error("an equal-generation advertisement must not replace")
	}
}

// OwningAdvertisement stores owned sorted and de-duplicated so equal sets are equal
// bytes.
func TestOwningAdvertisementSortsAndDeduplicates(t *testing.T) {
	advertisement := OwningAdvertisement("host-a", 1, []IP6{"fdab::3", "fdab::1", "fdab::3", "fdab::2"})
	if want := []IP6{"fdab::1", "fdab::2", "fdab::3"}; !reflect.DeepEqual(advertisement.Owned, want) {
		t.Fatalf("owned = %v, want sorted+deduped %v", advertisement.Owned, want)
	}
	if !advertisement.Owns("fdab::2") || advertisement.Owns("fdab::9") {
		t.Fatal("Owns must answer membership over the sorted set")
	}
}

func TestDedupeKeysNamespaceByKind(t *testing.T) {
	membership := MembershipDedupeKey(MembershipRecord{HostID: "host-a", Generation: 7})
	ownership := OwnershipDedupeKey(OwningAdvertisement("host-a", 7, nil))
	if membership == ownership {
		t.Fatal("membership and ownership keys at the same origin+generation must not collide")
	}
	if membership.Kind != "membership" || ownership.Kind != "ownership" {
		t.Fatalf("dedupe kinds wrong: %q / %q", membership.Kind, ownership.Kind)
	}
}
