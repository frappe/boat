// The memberlist.AliveDelegate trust gate (spec §19.4 / §19.5, design §3/§7).
//
// NotifyAlive is memberlist's per-node admission filter: return an error and the node
// is refused as a peer. This is where ANCP's identity model rides on top of
// memberlist's transport trust. A node is admitted only if its signed Meta verifies
// against:
//
//   - the ESTABLISHED signing key already anchored for its host_id (seed §19.4 or a
//     prior TOFU), for a host we already know; or
//   - a valid operator INTRODUCTION certificate (§19.5) over {host_id,
//     signing_public_key, generation}, for a first-contact newcomer — after which the
//     self-asserted key is TOFU-persisted and becomes the established key.
//
// A known host that self-asserts a DIFFERENT key than the one anchored is refused:
// wire self-rotation is closed, exactly as the Python envelope verifier closed it.
// Rotating a host's signing key means re-seeding it (operator pushes a new seed) or
// re-introducing it, never a peer swapping its own key mid-flight — which is the
// forge a compromised-but-authenticated host would attempt.
package networkd

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/hashicorp/memberlist"
)

// introductionGeneration is the fixed generation the operator binds in a §19.5
// introduction certificate ({host_id, signing_public_key, generation}), matching the
// controller's provision-time signing. The parent should confirm this against the
// Atlas controller's introduction-cert body before dogfooding cross-cluster joins.
const introductionGeneration Generation = 1

// NotifyAlive admits or refuses a peer based on its signed Meta (see the package
// doc). It runs on a memberlist goroutine, so it takes the daemon mutex around every
// trust-directory read and TOFU write.
func (daemon *Daemon) NotifyAlive(peer *memberlist.Node) error {
	if peer.Name == daemon.identity.HostID {
		return nil // our own alive message, emitted during setAlive — always accept
	}
	meta, err := decodeNodeMeta(peer.Meta)
	if err != nil {
		return fmt.Errorf("peer %s: undecodable Meta: %w", peer.Name, err)
	}
	// Injection guard at the doorstep: the render interpolates wg_public_key,
	// endpoint and mesh_address verbatim, and a signature authenticates WHO not WHAT
	// (a compromised host signs its own forged field values), so reject a Meta whose
	// fields carry whitespace or control chars before it can reach a render.
	candidate := MembershipRecord{
		HostID:             peer.Name,
		Endpoint:           peer.Addr.String(),
		WireGuardPublicKey: meta.WireGuardPublicKey,
		MeshAddress:        meta.MeshAddress,
	}
	if err := candidate.Validate(); err != nil {
		return fmt.Errorf("peer %s: %w", peer.Name, err)
	}

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	established, known := daemon.trust[peer.Name]
	if known && established != "" {
		if meta.SigningPublicKey != established {
			return fmt.Errorf(
				"peer %s self-asserts a signing key that differs from its anchored key "+
					"(wire self-rotation refused; re-seed or re-introduce to rotate)", peer.Name)
		}
		if err := verifyMetaSignature(meta, established); err != nil {
			return fmt.Errorf("peer %s: Meta signature does not verify against its anchored key: %w", peer.Name, err)
		}
		return nil
	}

	// First contact — the §19.5 introduction path.
	if daemon.operatorPublicKey == "" {
		// Dev/test posture: no operator trust root, so TOFU the self-asserted key on
		// a valid proof-of-possession, loudly. Production always sets an operator key.
		if err := verifyMetaSignature(meta, meta.SigningPublicKey); err != nil {
			return fmt.Errorf("peer %s: Meta self-signature invalid: %w", peer.Name, err)
		}
		slog.Warn("atlas-networkd: TOFU-trusting a first-contact peer with NO operator key configured",
			"host_id", peer.Name)
		daemon.tofuLearnLocked(peer.Name, meta.SigningPublicKey)
		return nil
	}
	if meta.IntroductionSignature == "" {
		return fmt.Errorf("peer %s is unknown and carries no operator introduction certificate", peer.Name)
	}
	introductionBody := map[string]any{
		"host_id":            peer.Name,
		"signing_public_key": meta.SigningPublicKey,
		"generation":         introductionGeneration,
	}
	if err := VerifyIntroduction(introductionBody, meta.IntroductionSignature, daemon.operatorPublicKey); err != nil {
		return fmt.Errorf("peer %s: introduction certificate does not verify against the operator key: %w", peer.Name, err)
	}
	if err := verifyMetaSignature(meta, meta.SigningPublicKey); err != nil {
		return fmt.Errorf("peer %s: Meta self-signature invalid: %w", peer.Name, err)
	}
	daemon.tofuLearnLocked(peer.Name, meta.SigningPublicKey)
	return nil
}

// tofuLearnLocked records a learned signing key into BOTH the runtime trust directory
// and the durable AppliedState map, so a restart re-trusts an introduced peer instead
// of demanding a fresh introduction (the one-sided-partition bug §19.5 / M6). The
// caller holds the daemon mutex.
func (daemon *Daemon) tofuLearnLocked(hostID HostID, signingPublicKey string) {
	daemon.trust[hostID] = signingPublicKey
	daemon.state.SigningPubkeys()[hostID] = signingPublicKey
}

// decodeNodeMeta parses a peer's Meta blob. An empty Meta is an error: every host in
// the fleet publishes one, so an empty blob is a peer that should not be admitted.
func decodeNodeMeta(raw []byte) (nodeMeta, error) {
	if len(raw) == 0 {
		return nodeMeta{}, fmt.Errorf("empty Meta")
	}
	var meta nodeMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nodeMeta{}, err
	}
	if meta.SigningPublicKey == "" || meta.Signature == "" {
		return nodeMeta{}, fmt.Errorf("Meta missing signing_public_key or signature")
	}
	return meta, nil
}
