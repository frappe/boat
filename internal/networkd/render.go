// Render the canonical wg-mesh config from the effective tables, ported from
// render.py (spec §16.2). A pure string function: given the host's own id, the
// effective Membership records, and the effective Ownership table, produce the
// wg-mesh.conf body the apply pipeline writes and feeds to `wg syncconf`.
//
// Byte-canonical — peers sorted by pubkey, AllowedIPs sorted — so the "in sync?"
// check is a plain string compare, and a host migrated from the controller-side
// render reads as in-sync on the first ANCP render of the same state.
//
// The §16.3 non-overlap invariant (Issue B): within one rendered config, no /128
// appears in more than one peer's AllowedIPs. Guaranteed three ways:
//
//  1. Each /128 in OwnerOf has exactly one owner, so it lands in one peer's set; a
//     /128 in Conflicts is dropped entirely (no peer advertises it) — the safe
//     default on a §7.3 conflict.
//  2. Each member's own mesh_address/128 is folded into the SAME cross-peer overlap
//     accounting as owned /128s BEFORE the config is emitted, so a mesh_address that
//     collides with another peer's owned /128 or mesh_address (a compromised host, or
//     an honest birthday collision) is dropped from ALL peers, never misdelivered.
//  3. The render is a whole-table recompute the apply pipeline applies as a single
//     atomic `wg syncconf`, never incrementally per-peer.
//
// Observer-local suspicion is NOT a render input: suspect peers stay routed (spec
// §14 keeps a partitioned host's VMs reachable during the suspicion window); only
// dead-past-dead_grace records are removed upstream, so they never reach here.
package networkd

import (
	"fmt"
	"sort"
	"strings"
)

// The config carries ListenPort only — the PrivateKey lives in its own 0600 file,
// never in this body, and the apply pipeline sets it LAST after syncconf (flipping
// that order clears the interface key and silently kills every tunnel).
const (
	wireGuardHostPort = 51820
	keepaliveSeconds  = 25
)

// RenderWireGuardDesired is the canonical wg-mesh.conf body for selfHostID (spec
// §16.2). A thin wrapper over RenderWireGuardDesiredWithConflicts that discards the
// render-level conflict map, for callers that only want the config body.
func RenderWireGuardDesired(selfHostID HostID, members map[HostID]MembershipRecord, ownership OwnershipTable) (string, error) {
	body, _, err := RenderWireGuardDesiredWithConflicts(selfHostID, members, ownership)
	return body, err
}

// RenderWireGuardDesiredWithConflicts returns the config body AND the render-level
// conflict map {private_ip: origins} — the H2 mesh_address collisions dropped in
// THIS render (a /128 that would have landed under more than one peer as an owned
// /128, a mesh_address, or one of each). These are NOT in ownership.Conflicts, which
// only carries owned-/128 double-ownership; the daemon unions the two before
// surfacing them (spec §7.3 / §18.2 — "report loudly"). The origins are the
// host_ids whose per-peer AllowedIPs contended for the /128.
//
// One [Peer] per OTHER member: AllowedIPs is the sorted set of /128s that member
// owns (per the effective table) plus that member's own infra mesh /128, with any
// /128 that would land under more than one peer dropped from all of them. selfHostID
// is skipped (a host never peers with itself), as is any member whose wg_public_key
// is still empty (a seed entry before the host's key has propagated). Every emitted
// record is re-validated at the doorstep. The body carries a single trailing newline
// and is byte-canonical.
func RenderWireGuardDesiredWithConflicts(
	selfHostID HostID, members map[HostID]MembershipRecord, ownership OwnershipTable,
) (string, map[IP6][]HostID, error) {
	allowedByPeer := allowedSetsByPeer(members, ownership)
	renderConflicts, contenders := foldOverlaps(allowedByPeer)
	if err := assertNoInputOverlap(allowedByPeer, ownership, renderConflicts); err != nil {
		return "", nil, err
	}
	body, err := assemble(selfHostID, members, allowedByPeer)
	if err != nil {
		return "", nil, err
	}
	return body, contenders, nil
}

// allowedSetsByPeer builds each member's raw /128 set from the effective ownership
// table PLUS its own mesh_address/128, before any overlap folding. An owner that is
// no longer a member (reaped upstream) contributes nothing — anti-entropy / GC will
// reconcile; we never invent a peer.
func allowedSetsByPeer(members map[HostID]MembershipRecord, ownership OwnershipTable) map[HostID]map[IP6]struct{} {
	allowed := make(map[HostID]map[IP6]struct{}, len(members))
	for hostID := range members {
		allowed[hostID] = map[IP6]struct{}{}
	}
	for ip, owner := range ownership.OwnerOf {
		if set, present := allowed[owner]; present {
			set[ip+"/128"] = struct{}{}
		}
	}
	// mesh_address is an author-controlled field on a signed record, and only
	// whitespace/control chars are validated — so a compromised-but-authenticated
	// host (or an honest birthday collision at ~320 hosts) can put a victim's /128
	// in its mesh_address. Folding it into the SAME overlap accounting as owned
	// /128s (below) is what stops it landing in two peers' AllowedIPs; appending it
	// AFTER the check was the old misdelivery bug.
	for hostID, peer := range members {
		allowed[hostID][peer.MeshAddress+"/128"] = struct{}{}
	}
	return allowed
}

// foldOverlaps removes every /128 that appears under more than one peer — whether it
// got there as an owned /128, a mesh_address, or one of each — dropping it from EVERY
// peer (never emit an overlapping /128, spec §16.3). It returns the dropped /128s as
// a set (with the /128 suffix, for the assertion) and the reportable conflict map
// {bare_ip: contending host_ids} the daemon surfaces to the operator.
func foldOverlaps(allowedByPeer map[HostID]map[IP6]struct{}) (map[IP6]struct{}, map[IP6][]HostID) {
	placements := map[IP6][]HostID{}
	for hostID, ips := range allowedByPeer {
		for ip := range ips {
			placements[ip] = append(placements[ip], hostID)
		}
	}
	dropped := map[IP6]struct{}{}
	contenders := map[IP6][]HostID{}
	for ip, holders := range placements {
		if len(holders) <= 1 {
			continue
		}
		dropped[ip] = struct{}{}
		// The reported private_ip is the bare /128 (matching the owned-conflict
		// shape); the dropped set keeps the suffix because that is the AllowedIPs
		// form it is subtracted from.
		bare := strings.TrimSuffix(ip, "/128")
		contenders[bare] = sortedUnique(holders)
	}
	for _, ips := range allowedByPeer {
		for ip := range dropped {
			delete(ips, ip)
		}
	}
	return dropped, contenders
}

// assemble emits the byte-canonical config: [Interface] then one [Peer] per other
// member, peers sorted by pubkey (the same key the controller-side render uses, so
// the two byte-compare in the same state). Every emitted record is re-validated —
// belt-and-suspenders on top of the parse-boundary Validate — so a directly
// constructed record can never inject a [Peer] directive through a newline.
func assemble(selfHostID HostID, members map[HostID]MembershipRecord, allowedByPeer map[HostID]map[IP6]struct{}) (string, error) {
	lines := []string{"[Interface]", fmt.Sprintf("ListenPort = %d", wireGuardHostPort), ""}
	for _, peer := range peersByPublicKey(members) {
		if peer.HostID == selfHostID {
			continue // a host never peers with itself
		}
		if peer.WireGuardPublicKey == "" {
			continue // key not yet propagated (a seed entry before first gossip)
		}
		if err := peer.Validate(); err != nil {
			return "", err
		}
		allowed := sortedSet(allowedByPeer[peer.HostID])
		lines = append(lines,
			"[Peer]",
			fmt.Sprintf("PublicKey = %s", peer.WireGuardPublicKey),
			fmt.Sprintf("AllowedIPs = %s", strings.Join(allowed, ", ")),
			fmt.Sprintf("Endpoint = [%s]:%d", peer.Endpoint, wireGuardHostPort),
			fmt.Sprintf("PersistentKeepalive = %d", keepaliveSeconds),
			"",
		)
	}
	return strings.Join(lines, "\n") + "\n", nil
}

// peersByPublicKey returns the members sorted by wg_public_key for byte-canonical
// output.
func peersByPublicKey(members map[HostID]MembershipRecord) []MembershipRecord {
	peers := make([]MembershipRecord, 0, len(members))
	for _, peer := range members {
		peers = append(peers, peer)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].WireGuardPublicKey < peers[j].WireGuardPublicKey })
	return peers
}

// sortedSet is a set rendered as a sorted slice — the canonical per-peer AllowedIPs.
func sortedSet(set map[IP6]struct{}) []IP6 {
	values := make([]IP6, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

// assertNoInputOverlap proves the §16.3 invariant on the FINAL per-peer AllowedIPs —
// owned /128s AND each peer's mesh_address/128, overlaps already dropped. No /128 may
// appear under more than one peer; a dropped render conflict must appear under zero;
// and Conflicts must be disjoint from OwnerOf. render.py enforced this with `assert`;
// Boat returns an error instead (no panic in library code, Taste.md), so a future
// bug in the effective-table computation or the mesh_address fold is caught loud
// before it reaches the wire rather than crashing a daemon that supervises live VMs.
// It is exercised directly as a test invariant.
func assertNoInputOverlap(allowedByPeer map[HostID]map[IP6]struct{}, ownership OwnershipTable, renderConflicts map[IP6]struct{}) error {
	placed := map[IP6]HostID{}
	for hostID, ips := range allowedByPeer {
		for ip := range ips {
			if prior, seen := placed[ip]; seen {
				return fmt.Errorf("render input overlap: %s placed for both %s and %s", ip, prior, hostID)
			}
			placed[ip] = hostID
		}
	}
	for ip := range renderConflicts {
		if _, leaked := placed[ip]; leaked {
			return fmt.Errorf("dropped render conflict leaked back into a peer: %s", ip)
		}
	}
	for ip := range ownership.Conflicts {
		if _, elected := ownership.OwnerOf[ip]; elected {
			return fmt.Errorf("conflicting /128 leaked into owner_of: %s", ip)
		}
	}
	return nil
}
