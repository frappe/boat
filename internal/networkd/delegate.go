// The memberlist.Delegate seam (spec §3 of the WO-5 design). One *Daemon implements
// Delegate here, EventDelegate in events.go, and AliveDelegate in trust.go — the
// three memberlist callback surfaces, all fronting the same AppliedState + render/
// apply pipeline under one mutex.
//
// memberlist owns the four channels the ANCP wire used to carry by hand:
//
//   - NodeMeta — this host's small signed {wg_public_key, mesh_address,
//     signing_public_key} blob, re-sent on every alive/refute. It replaces the
//     per-host MembershipRecord on the wire; the endpoint rides memberlist's own
//     advertise address and the generation rides its incarnation number.
//   - NotifyMsg / GetBroadcasts — the OwnershipAdvertisement broadcast stream (gossip
//     fan-out), backed by a TransmitLimitedQueue.
//   - LocalState / MergeRemoteState — the FULL ownership stream over push/pull, the
//     anti-entropy backstop independent of broadcast delivery.
//
// Every OwnershipAdvertisement is verified against the ORIGIN's signing key (from the
// seed-anchored + TOFU trust directory) before it is applied — memberlist's optional
// AES keyring authenticates "a cluster member sent this" but does NOT bind a specific
// origin to a record, which is exactly ANCP's threat model (spec §19 / design §8.2).
package networkd

import (
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/hashicorp/memberlist"
)

// Compile-time proof the Daemon satisfies the three memberlist callback interfaces.
var (
	_ memberlist.Delegate      = (*Daemon)(nil)
	_ memberlist.EventDelegate = (*Daemon)(nil)
	_ memberlist.AliveDelegate = (*Daemon)(nil)
)

// metaKind is the domain-separation tag folded into the signed Meta body, so a Meta
// signature can never be replayed as a membership or ownership signature (their
// canonical bodies carry different field sets anyway, but the tag is explicit).
const metaKind = "meta"

// nodeMeta is the signed blob NodeMeta emits and every peer's Meta decodes to. It is
// deliberately tiny (well under memberlist's 512-byte MetaMaxSize): three base64/IPv6
// fields, an ed25519 self-signature proving possession of signing_public_key's
// private half, and — only on a newcomer joining an existing cluster — the operator
// introduction certificate (§19.5). The endpoint is NOT here: it is memberlist's
// advertise address, read off the peer Node.
type nodeMeta struct {
	WireGuardPublicKey    string `json:"wg_public_key"`
	MeshAddress           string `json:"mesh_address"`
	SigningPublicKey      string `json:"signing_public_key"`
	IntroductionSignature string `json:"introduction_signature,omitempty"`
	Signature             string `json:"sig"`
}

// canonicalMetaBody is the exact bytes a Meta signature commits to — the three
// identity fields plus the metaKind tag, encoded by the same self-consistent
// encoder sign/verify both run (json.Marshal alphabetizes keys and emits compact
// separators). The self-asserted signing_public_key is inside the signed body, so a
// relay cannot swap it without breaking the signature.
func canonicalMetaBody(wireGuardPublicKey, meshAddress, signingPublicKey string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"_kind":              metaKind,
		"wg_public_key":      wireGuardPublicKey,
		"mesh_address":       meshAddress,
		"signing_public_key": signingPublicKey,
	})
}

// encodeOwnNodeMeta builds and signs this host's Meta blob. The fields are immutable
// after construction, so no lock is needed. The signature is over the identity body
// with our own signing key; a peer verifies it against the key it has anchored for us
// (seed or TOFU), and a newcomer's peers verify our introduction cert against the
// operator key.
func (daemon *Daemon) encodeOwnNodeMeta() ([]byte, error) {
	body, err := canonicalMetaBody(daemon.wireGuardPublicKey, daemon.identity.MeshAddress, daemon.signingPublicKey)
	if err != nil {
		return nil, err
	}
	signature, err := SignDetached(body, daemon.signingPrivateKey)
	if err != nil {
		return nil, err
	}
	return json.Marshal(nodeMeta{
		WireGuardPublicKey:    daemon.wireGuardPublicKey,
		MeshAddress:           daemon.identity.MeshAddress,
		SigningPublicKey:      daemon.signingPublicKey,
		IntroductionSignature: daemon.introductionSignature,
		Signature:             signature,
	})
}

// verifyMetaSignature checks a decoded peer Meta's self-signature against a chosen
// signing key — the ESTABLISHED (anchored) key for a known peer, or the self-asserted
// key for a first-contact newcomer whose introduction cert has already been operator-
// verified. Either way the check proves the peer holds the private half of that key.
func verifyMetaSignature(meta nodeMeta, signingPublicKey string) error {
	body, err := canonicalMetaBody(meta.WireGuardPublicKey, meta.MeshAddress, meta.SigningPublicKey)
	if err != nil {
		return err
	}
	return VerifyDetached(body, meta.Signature, signingPublicKey)
}

// NodeMeta returns this host's signed Meta blob, asserting it fits memberlist's
// limit. It never should exceed ~220 bytes; if a future field pushed it past the
// bound we return nil (a peer then rejects us — a loud, non-fatal failure) rather
// than let memberlist panic on an over-long blob.
func (daemon *Daemon) NodeMeta(limit int) []byte {
	meta, err := daemon.encodeOwnNodeMeta()
	if err != nil {
		slog.Error("atlas-networkd: could not encode NodeMeta", "error", err)
		return nil
	}
	if len(meta) > limit {
		slog.Error("atlas-networkd: NodeMeta exceeds the memberlist limit; advertising empty meta",
			"size", len(meta), "limit", limit)
		return nil
	}
	return meta
}

// NotifyMsg decodes one broadcast OwnershipAdvertisement, verifies it against the
// origin's trusted signing key, and applies the §13.2 monotonic rule — scheduling a
// debounced apply on a change. memberlist calls this on the receive path, so it must
// not block; the work is a decode, a verify and a map update.
func (daemon *Daemon) NotifyMsg(message []byte) {
	advertisement, err := decodeOwnership(message)
	if err != nil {
		return // a malformed user message — ignore, never crash the receive loop
	}
	daemon.applyOwnershipFromWire(advertisement)
}

// GetBroadcasts hands memberlist the queued OwnershipAdvertisements to piggyback on
// the next gossip packet. The TransmitLimitedQueue owns retransmit counting and the
// per-origin invalidation (a newer full set supersedes the older queued one).
func (daemon *Daemon) GetBroadcasts(overhead, limit int) [][]byte {
	return daemon.broadcasts.GetBroadcasts(overhead, limit)
}

// LocalState marshals the full per-origin ownership stream (signatures included) for
// a push/pull sync — the correctness backstop that does not depend on any single
// broadcast being delivered.
func (daemon *Daemon) LocalState(join bool) []byte {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	stream := make([]ownershipWire, 0, len(daemon.state.Ownership()))
	for _, advertisement := range daemon.state.Ownership() {
		stream = append(stream, toOwnershipWire(advertisement))
	}
	encoded, err := json.Marshal(stream)
	if err != nil {
		slog.Error("atlas-networkd: could not encode LocalState", "error", err)
		return nil
	}
	return encoded
}

// MergeRemoteState verifies and applies every advertisement in a peer's pushed
// ownership stream (each against its origin's trusted key, each under the monotonic
// rule), then schedules one apply if anything changed.
func (daemon *Daemon) MergeRemoteState(buffer []byte, join bool) {
	var stream []ownershipWire
	if err := json.Unmarshal(buffer, &stream); err != nil {
		return
	}
	for _, wire := range stream {
		daemon.applyOwnershipFromWire(fromOwnershipWire(wire))
	}
}

// applyOwnershipFromWire is the shared verify-then-apply path for both the broadcast
// (NotifyMsg) and the push/pull (MergeRemoteState) channels. A dedupe-cache hit
// short-circuits before any crypto; an origin whose signing key we do not yet trust
// is DEFERRED, not applied — dropped and left to be re-pulled once its Meta anchors
// the key, so an authenticated relay cannot inject a phantom /128 (a §7.3 conflict)
// nor advance the origin's generation past the authentic signed record.
func (daemon *Daemon) applyOwnershipFromWire(advertisement OwnershipAdvertisement) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if daemon.state.SeenAlready(OwnershipDedupeKey(advertisement)) {
		return
	}
	trustedKey, known := daemon.trust[advertisement.Origin]
	if !known || trustedKey == "" {
		daemon.counters.incr("ownership_deferred_no_key")
		return
	}
	if err := VerifyOwnership(advertisement, trustedKey); err != nil {
		if errors.Is(err, ErrSignature) {
			daemon.counters.incr("signature_failed")
		}
		return
	}
	if daemon.state.ApplyOwnership(advertisement) {
		daemon.scheduleApplyLocked(daemon.nowFn())
	}
}

// ownershipWire is the JSON shape an OwnershipAdvertisement rides in over both the
// broadcast queue and the push/pull stream: the origin, its generation, the sorted
// owned set, and the origin's ed25519 signature over that body.
type ownershipWire struct {
	Origin     HostID     `json:"origin"`
	Generation Generation `json:"generation"`
	Owned      []IP6      `json:"owned"`
	Signature  string     `json:"signature"`
}

func toOwnershipWire(advertisement OwnershipAdvertisement) ownershipWire {
	return ownershipWire{
		Origin:     advertisement.Origin,
		Generation: advertisement.Generation,
		Owned:      advertisement.Owned,
		Signature:  advertisement.Signature,
	}
}

// fromOwnershipWire rebuilds the advertisement through OwningAdvertisement so Owned
// is re-sorted and de-duplicated into the SAME canonical form the origin signed —
// otherwise a reordered owned set would fail VerifyOwnership even with a valid sig.
func fromOwnershipWire(wire ownershipWire) OwnershipAdvertisement {
	advertisement := OwningAdvertisement(wire.Origin, wire.Generation, wire.Owned)
	advertisement.Signature = wire.Signature
	return advertisement
}

func encodeOwnership(advertisement OwnershipAdvertisement) ([]byte, error) {
	return json.Marshal(toOwnershipWire(advertisement))
}

func decodeOwnership(message []byte) (OwnershipAdvertisement, error) {
	var wire ownershipWire
	if err := json.Unmarshal(message, &wire); err != nil {
		return OwnershipAdvertisement{}, err
	}
	return fromOwnershipWire(wire), nil
}

// ownershipBroadcast is one queued OwnershipAdvertisement. It is a NamedBroadcast
// keyed on the origin, so enqueuing this host's newer full set invalidates its own
// older queued set — memberlist never gossips two generations of one origin's
// ownership at once.
type ownershipBroadcast struct {
	origin  HostID
	message []byte
}

func (broadcast *ownershipBroadcast) Invalidates(other memberlist.Broadcast) bool {
	previous, ok := other.(*ownershipBroadcast)
	return ok && previous.origin == broadcast.origin
}

func (broadcast *ownershipBroadcast) Message() []byte { return broadcast.message }
func (broadcast *ownershipBroadcast) Finished()       {}
func (broadcast *ownershipBroadcast) Name() string    { return broadcast.origin }
