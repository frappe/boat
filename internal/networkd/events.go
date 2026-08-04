// The memberlist.EventDelegate seam + the ownership_grace retention timer (design
// §3.4 — the #1 nuance memberlist does NOT provide).
//
// memberlist gives an ordered join/update/leave stream with each peer's current Meta.
// Membership monotonicity is memberlist's own job (incarnation numbers), so the
// records this file feeds AppliedState carry a synthetic ever-increasing generation
// per peer — enough for the §13.2 replace rule to always take the latest Meta, since
// memberlist already resolved which "latest" is.
//
// ownership_grace is the piece memberlist will not do: it removes a dead node
// immediately, but ANCP keeps a dead host's ownership /128s routable for
// ownership_grace (60s, deliberately longer than memberlist's own suspicion + a
// dead_grace) so a late-refuting host does not blackhole its own VMs. On NotifyLeave
// this stamps deadAt and moves the host's record into the render-only routable_dead
// view (so its [Peer] keeps rendering); the loop's GC reaps it only once
// now-deadAt ≥ ownership_grace; a rejoin/update clears the timer.
package networkd

import "github.com/hashicorp/memberlist"

// NotifyJoin re-reads the joined peer's Meta and applies its membership record, so
// its [Peer] renders and its owned /128s (verified separately) route to it.
func (daemon *Daemon) NotifyJoin(node *memberlist.Node) {
	daemon.upsertMembershipFromNode(node)
}

// NotifyUpdate re-reads a peer's changed Meta (a wg-key or mesh-address change) and
// re-renders. The signing-key trust directory is NOT rotated here — NotifyAlive
// already refused any wire self-rotation, so the anchored key stands.
func (daemon *Daemon) NotifyUpdate(node *memberlist.Node) {
	daemon.upsertMembershipFromNode(node)
}

// NotifyLeave stamps the ownership_grace clock and moves the peer into the render-only
// routable_dead view, keeping its [Peer] alive during the late-refute window. The GC
// tick reaps it once the window elapses. A rejoin (NotifyJoin/Update) clears the
// timer before then.
func (daemon *Daemon) NotifyLeave(node *memberlist.Node) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.state.MarkRoutableDead(node.Name)
	if _, pending := daemon.deadAt[node.Name]; !pending {
		daemon.deadAt[node.Name] = daemon.nowFn()
	}
	daemon.scheduleApplyLocked(daemon.nowFn())
}

// upsertMembershipFromNode builds a MembershipRecord from a peer node's Meta + its
// advertise address (the endpoint) and applies it under a synthetic monotonic
// generation, so the record always takes. A change clears any pending ownership_grace
// timer for the host (a (re)join is a refute) and schedules a re-render.
func (daemon *Daemon) upsertMembershipFromNode(node *memberlist.Node) {
	if node.Name == daemon.identity.HostID {
		return // a host never renders itself as a peer
	}
	meta, err := decodeNodeMeta(node.Meta)
	if err != nil {
		return // NotifyAlive already gated admission; a decode miss here is defensive
	}

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	daemon.peerGeneration[node.Name]++
	record := MembershipRecord{
		HostID:             node.Name,
		Kind:               MembershipKindMember,
		State:              MemberStateAlive,
		Endpoint:           node.Addr.String(),
		WireGuardPublicKey: meta.WireGuardPublicKey,
		MeshAddress:        meta.MeshAddress,
		Generation:         daemon.peerGeneration[node.Name],
		SigningPublicKey:   meta.SigningPublicKey,
	}
	if daemon.state.ApplyMembership(record, nil) {
		delete(daemon.deadAt, node.Name)
		daemon.scheduleApplyLocked(daemon.nowFn())
	}
}

// reapDeadOrigins is the ownership_grace GC tick: for every host declared dead, once
// now-deadAt ≥ ownership_grace, drop its ownership advertisement AND its render-only
// record (spec §14.3/§14.6) and forget the timer. Returns whether anything changed,
// so the loop can schedule an apply and persist. Runs under the daemon mutex held by
// the caller.
func (daemon *Daemon) reapDeadOriginsLocked(now float64) bool {
	changed := false
	for hostID, deadAt := range daemon.deadAt {
		if now-deadAt < daemon.config.OwnershipGraceSeconds {
			continue
		}
		// GCOrigin drops both the ownership advertisement and the routable_dead
		// record; even a host with only a routable_dead record (no ownership) loses
		// its [Peer] here, which is a render change.
		daemon.state.GCOrigin(hostID)
		delete(daemon.deadAt, hostID)
		delete(daemon.peerGeneration, hostID)
		changed = true
	}
	return changed
}
