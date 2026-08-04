// The in-memory applied state, ported from state.py (spec §13.3, §14.3, §14.5).
//
// AppliedState holds the inputs to the effective tables — the latest Membership
// Record and Ownership Advertisement per origin — plus this host's own generation
// counter, the duplicate-suppression cache, and the routable_dead retention set. The
// effective tables themselves are NOT stored: they are derived from these inputs on
// demand (EffectiveOwnership + the membership map), so only the inputs are kept.
//
// Disk persistence is the daemon's to wire (state.py's to_dict/from_dict + atomic
// save land in a later piece); this is the pure logic + apply methods it drives.
//
// Two nuances memberlist does not give us live here:
//
//   - The §13.3 dedupe cache is a bounded LRU keyed on (origin, kind, generation).
//     A hit short-circuits verify + apply + forward, and refreshes the key's LRU
//     position so a still-arriving duplicate is not evicted mid partition-heal.
//   - The §14.3 routable_dead set keeps a dead host's Membership Record in a
//     render-only view after its live record is reaped at dead_grace, so its /128s
//     stay routable through the longer ownership_grace window and a late-refuting
//     host does not blackhole its own VMs.
package networkd

import "container/list"

// AppliedState is the applied tables + dedupe cache (spec §13.3, §14.5). All fields
// are unexported; the daemon reaches them through the methods and accessors so the
// apply rules and the LRU stay the single source of truth.
type AppliedState struct {
	membership     map[HostID]MembershipRecord
	ownership      map[HostID]OwnershipAdvertisement
	routableDead   map[HostID]MembershipRecord
	signingPubkeys map[HostID]string
	seen           *seenCache
	ownGeneration  Generation
}

// NewAppliedState is a fresh, empty state with the dedupe cache bounded at
// seenCapacity (state.py's 10_000 default when the caller passes 0).
func NewAppliedState(seenCapacity int) *AppliedState {
	if seenCapacity <= 0 {
		seenCapacity = 10_000
	}
	return &AppliedState{
		membership:     map[HostID]MembershipRecord{},
		ownership:      map[HostID]OwnershipAdvertisement{},
		routableDead:   map[HostID]MembershipRecord{},
		signingPubkeys: map[HostID]string{},
		seen:           newSeenCache(seenCapacity),
	}
}

// ApplyMembership applies the §13.2 rule: replace iff the incoming generation is
// strictly higher than the existing record's for this origin. Returns whether the
// table changed. The dedupe key is recorded regardless — a same-generation
// re-delivery is a no-op apply but is still cached so the byte-equal record is not
// forwarded again.
//
// When pubkeyCache is non-nil and the incoming record carries a signing pubkey, the
// cache is updated so the record verifier can find the latest key for this origin —
// which is how key rotation via a higher-generation signed record propagates. On a
// change, a stale routable_dead record for a now-live host is dropped so the fresh
// record is not carried alongside the dead one.
func (state *AppliedState) ApplyMembership(incoming MembershipRecord, pubkeyCache map[HostID]string) bool {
	existing, present := state.membership[incoming.HostID]
	changed := MembershipReplaces(existingPointer(existing, present), incoming)
	if changed {
		state.membership[incoming.HostID] = incoming
		delete(state.routableDead, incoming.HostID)
		if pubkeyCache != nil && incoming.SigningPublicKey != "" {
			pubkeyCache[incoming.HostID] = incoming.SigningPublicKey
		}
	}
	state.seen.mark(MembershipDedupeKey(incoming))
	return changed
}

// ApplyOwnership applies the §13.2 rule for an Ownership Advertisement — the same
// per-origin monotonic rule. Returns whether the table changed.
func (state *AppliedState) ApplyOwnership(incoming OwnershipAdvertisement) bool {
	existing, present := state.ownership[incoming.Origin]
	changed := OwnershipReplaces(ownershipPointer(existing, present), incoming)
	if changed {
		state.ownership[incoming.Origin] = incoming
	}
	state.seen.mark(OwnershipDedupeKey(incoming))
	return changed
}

// SeenAlready checks the §13.3 dedupe cache; a hit refreshes the key's LRU position.
// The apply path gates verify + apply + forward on this before doing any work.
func (state *AppliedState) SeenAlready(key DedupeKey) bool {
	return state.seen.seen(key)
}

// BumpOwnGeneration increments this host's own per-origin generation counter and
// returns the new value to advertise. Persisted so a crash-restart produces
// persisted+1, never 1 (a stale low-gen record must never overwrite a peer's newer
// view).
func (state *AppliedState) BumpOwnGeneration() Generation {
	state.ownGeneration++
	return state.ownGeneration
}

// OwnGeneration is the current own-generation counter, for the daemon that persists
// and advertises it.
func (state *AppliedState) OwnGeneration() Generation { return state.ownGeneration }

// SetOwnGeneration restores the counter loaded from disk on restart.
func (state *AppliedState) SetOwnGeneration(generation Generation) { state.ownGeneration = generation }

// MarkRoutableDead moves a host's live Membership Record into the render-only
// routable_dead view — the daemon calls this when it reaps membership at dead_grace,
// so the dead host is gone from every gossip / probe / anti-entropy path (all of
// which read the membership map) while its [Peer] keeps carrying its /128s until
// ownership_grace. A no-op for a host with no live record. Returns whether a record
// was moved.
func (state *AppliedState) MarkRoutableDead(hostID HostID) bool {
	record, present := state.membership[hostID]
	if !present {
		return false
	}
	delete(state.membership, hostID)
	state.routableDead[hostID] = record
	return true
}

// GCOrigin drops an origin's ownership advertisement and its render-only
// routable_dead record (spec §14.6): once its ownership is gone there is nothing left
// to route to it, so its [Peer] must disappear too. Returns whether the origin's
// advertisement was present.
func (state *AppliedState) GCOrigin(origin HostID) bool {
	delete(state.routableDead, origin)
	_, present := state.ownership[origin]
	delete(state.ownership, origin)
	return present
}

// GCOriginIfDead reaps the origin's ownership advertisement iff ownership_grace has
// elapsed since the host was declared dead — the highest-risk behavioral nuance
// memberlist does not provide. deadAt is when the failure detector declared the host
// dead; the deadline is now - deadAt >= ownershipGrace. The window is deliberately
// longer than suspect_timeout + dead_grace (spec §14.3) so a late-refuting host does
// not lose its routes mid-refute. Returns whether the advertisement was reaped this
// call; the render-only record is dropped alongside it.
func (state *AppliedState) GCOriginIfDead(origin HostID, deadAt, ownershipGrace, now float64) bool {
	if _, present := state.ownership[origin]; !present {
		return false
	}
	if now-deadAt < ownershipGrace {
		return false
	}
	delete(state.ownership, origin)
	delete(state.routableDead, origin)
	return true
}

// Membership is the live per-origin latest Membership Record map — the gossip /
// probe / anti-entropy view.
func (state *AppliedState) Membership() map[HostID]MembershipRecord { return state.membership }

// Ownership is the live per-origin latest Ownership Advertisement map — the input to
// EffectiveOwnership.
func (state *AppliedState) Ownership() map[HostID]OwnershipAdvertisement { return state.ownership }

// SigningPubkeys is the TOFU-learned origin→signing-pubkey directory (spec §19.5),
// which the daemon merges into its verifier trust set on boot.
func (state *AppliedState) SigningPubkeys() map[HostID]string { return state.signingPubkeys }

// RenderMembers is the map the render consumes: the live records overlaid on the
// render-only routable_dead view, so a dead host's [Peer] keeps carrying its /128s
// during the ownership_grace window while a live record for the same host always
// wins. A fresh map, safe for the caller to hand straight to render.
func (state *AppliedState) RenderMembers() map[HostID]MembershipRecord {
	members := make(map[HostID]MembershipRecord, len(state.membership)+len(state.routableDead))
	for hostID, record := range state.routableDead {
		members[hostID] = record
	}
	for hostID, record := range state.membership {
		members[hostID] = record
	}
	return members
}

// existingPointer adapts a map lookup to the *record the replace rule expects — nil
// when absent, so "first record for this origin" reads as replace.
func existingPointer(record MembershipRecord, present bool) *MembershipRecord {
	if !present {
		return nil
	}
	return &record
}

func ownershipPointer(record OwnershipAdvertisement, present bool) *OwnershipAdvertisement {
	if !present {
		return nil
	}
	return &record
}

// seenCache is the §13.3 dedupe LRU: O(1) membership, insert, refresh and eviction
// via a map into a doubly-linked recency list. state.py used an OrderedDict for the
// same O(1) profile, avoiding the deque+set rebuild that made the old cache O(n) per
// record — a per-record DoS footgun over a 10k-entry cache.
type seenCache struct {
	capacity int
	index    map[DedupeKey]*list.Element
	recency  *list.List // front = most recently seen, back = eviction candidate
}

func newSeenCache(capacity int) *seenCache {
	return &seenCache{capacity: capacity, index: map[DedupeKey]*list.Element{}, recency: list.New()}
}

// seen reports whether key is cached, moving a hit to the front so a still-arriving
// duplicate is not evicted out from under an ongoing partition heal.
func (cache *seenCache) seen(key DedupeKey) bool {
	element, present := cache.index[key]
	if !present {
		return false
	}
	cache.recency.MoveToFront(element)
	return true
}

// mark records key, refreshing an existing key's recency or appending a fresh one
// and evicting the oldest once over capacity.
func (cache *seenCache) mark(key DedupeKey) {
	if element, present := cache.index[key]; present {
		cache.recency.MoveToFront(element)
		return
	}
	cache.index[key] = cache.recency.PushFront(key)
	for cache.recency.Len() > cache.capacity {
		oldest := cache.recency.Back()
		cache.recency.Remove(oldest)
		delete(cache.index, oldest.Value.(DedupeKey))
	}
}
