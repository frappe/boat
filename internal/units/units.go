// Package units supervises the host's own services — every unit on this machine
// that is not a VM.
//
// Boat already owns the per-VM `firecracker-vm@` instances, with a fence in
// front of them and a journal behind them. This package owns the other set: the
// LVM thin pool that must be rebound before a VM can find its disk, the network
// control plane, the wake trap, the fail-closed management firewall. It reports
// what systemd says about each, and it can bring one up. It reads none of their
// state and writes none of it — ANCP's gossip, membership and wg peer table are
// atlas-networkd's alone, and supervision here is lifecycle and nothing else
// (spec/33-boat.md §3.7, §0).
//
// # The verb set is start and restart, and there is no stop
//
// The obvious three are start, stop and restart, and the third of those is the
// one to leave out. Supervision means keeping this host's own services UP: "be
// running" and "be running afresh" are the only states a control plane ever
// wants the pool, the daemon, the trap or the firewall in, and `restart` already
// covers every legitimate stop-then-start — including the config re-apply the
// mesh unit's ExecStop exists for.
//
// A stop would be the verb with teeth and no driver. Nothing in Boat wants a
// sibling unit down: the reconciler reconciles VMs, so nothing would ever notice
// or undo it. What it would buy is the ability to strand every sleeping VM on
// the host by stopping the one process that watches their counters, or to
// black-hole the private plane for every guest. An operator who genuinely has to
// take one down has SSH break-glass, which the split keeps deliberately (§12).
//
// `reset-failed` is left out for a related reason. A unit that has tripped its
// StartLimitBurst refuses to start until it is cleared, and that refusal is the
// backstop working: a rate limit the rate-limited thing can reset is not one.
// The refusal is reported to the caller instead of routed around.
//
// # The supervised set is a closed list of literal names
//
// Not a prefix, not a glob, not "anything under /etc/systemd/system". A name is
// supervised if it is byte-for-byte one of the four below, and everything else
// — `sshd.service`, `boat.service`, and every `firecracker-vm@<uuid>.service` —
// is refused before a command is rendered. That is what makes "the per-VM units
// are not reachable through unit supervision" a property of the list rather than
// a check somebody has to remember: a UUID cannot be spelled with these four
// literals.
//
// The same names are pinned in sudoers.d/boat, one grant per unit per action,
// with no wildcard anywhere. Both layers are needed and neither is redundant:
// this one is the contract, that one is what holds if this one is wrong.
//
// # Reading needs no privilege; acting does
//
// `systemctl show` is a property read over the system bus that any user may
// make, so liveness costs no sudoers grant at all — verified on a real host as
// the unprivileged `boat` user. Only `start` and `restart` are privileged. That
// asymmetry is worth keeping: the endpoint Atlas polls every five minutes for
// every host in the fleet reaches nothing that needs root.
package units

import "slices"

// The units this Boat supervises. Every one is installed on a bootstrapped host
// by Atlas (Server.BOOTSTRAP_UPLOAD_SOURCES, or mgmt-firewall-confirm for the
// firewall), and every one is a HOST service.
//
// Two units in scripts/systemd/ are deliberately absent:
//
//   - `gateway.service` runs INSIDE the customer gateway guest — its own unit
//     file says so, and it is installed into a guest by
//     atlas/atlas/customer_gateway.py, never onto a host. Supervising it would
//     put Boat on the guest plane, which §7.1 keeps in Atlas over guest-SSH.
//   - `host-mesh.service` is the pre-ANCP predecessor of atlas-networkd, and
//     nothing installs it any more: it is in neither BOOTSTRAP_UPLOAD_SOURCES
//     nor on any host in the fleet (checked). Adding it back is two literal
//     lines here and two in sudoers, on the day a host has one.
const (
	poolUnit                = "atlas-pool.service"
	networkControlPlaneUnit = "atlas-networkd.service"
	wakeTrapUnit            = "atlas-wake-trap.service"
	managementFirewallUnit  = "atlas-mgmt-firewall.service"
)

// supervised is the closed list. Order is the order a liveness read reports
// them in, and it runs from the deepest dependency outward — the pool a VM's
// disk lives in, then the plane its packets cross, then the reflex that wakes
// it, then the firewall in front of the lot.
var supervised = []string{
	poolUnit,
	networkControlPlaneUnit,
	wakeTrapUnit,
	managementFirewallUnit,
}

// Supervised is the set, copied, so a caller cannot append to the allow-list by
// holding it.
func Supervised() []string { return slices.Clone(supervised) }

// IsSupervised reports whether this host will report on or act on a unit by
// this name. Exact match and nothing else: no suffix is added, no case is
// folded, no path is trimmed. A second spelling of a name is a second thing to
// get right, and sudoers.d/boat is a file-length argument about where that ends.
func IsSupervised(name string) bool { return slices.Contains(supervised, name) }

// Action is what Boat may ask of a sibling unit.
type Action string

const (
	// Start brings a unit up and is a no-op on one already up.
	Start Action = "start"
	// Restart brings a unit up again from scratch, which is how a oneshot
	// re-asserts what it asserts and how a daemon re-reads a pushed config.
	Restart Action = "restart"
)

// ParseAction reads the action off a request. It is a closed set, so an
// unrecognised value is refused rather than passed through to systemd — the
// generated server checks the shape of a body and not the membership of an
// enum, so this is where the enum in api/openapi.yaml is actually enforced.
func ParseAction(value string) (Action, bool) {
	switch Action(value) {
	case Start:
		return Start, true
	case Restart:
		return Restart, true
	}
	return "", false
}

// commandFor is the literal template each action renders through.
//
// A switch rather than "sudo systemctl " + string(action) + " {}". internal/run's
// trust model divides a command into a literal template and quoted parameters,
// and a template assembled from a value is a template a value can grow. Action
// is closed and both templates are written out, so the only thing that varies in
// what reaches the host is the unit name, in its quoted hole.
func commandFor(action Action) (string, bool) {
	switch action {
	case Start:
		return "sudo systemctl start {}", true
	case Restart:
		return "sudo systemctl restart {}", true
	}
	return "", false
}
