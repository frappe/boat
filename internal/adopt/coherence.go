package adopt

import (
	"fmt"
	"slices"
	"strings"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/paths"
)

// contradictions lists everything about one UUID's artifacts that cannot all be
// true of a single VM. An empty list means the set reads as exactly one VM and
// the UUID is adopted; anything else means it is quarantined and never adopted.
//
// There are two tiers, and which tier a check sits in is the whole judgement:
//
//   - DURABLE artifacts are what a VM is made of at rest — its directory, its
//     jail tree, its sidecar, its root disk. They survive a stop, a sleep and a
//     host reboot, so one of them missing while the others are present is a
//     torn-down VM caught half-way, whatever the unit says.
//   - RUNTIME artifacts — namespace, tap, host veth, API socket — exist only
//     between a start and a stop. They are required only of a unit that says it
//     is active, because that unit is claiming a Firecracker is up, and a
//     Firecracker cannot be up without them.
//
// What is deliberately NOT a contradiction: runtime artifacts left behind by a
// VM that is no longer running. A stop that skipped its ExecStopPost leaves the
// namespace and the proxy-NDP entry standing, which is untidy and worth fixing —
// but the VM's identity is not in doubt, its disk is where it should be, and
// booting it is safe. Quarantining it would hide a healthy VM from the
// controller, which is a cost this package pays only to avoid ambiguity.
func (artifacts hostArtifacts) contradictions() []string {
	if durable := artifacts.durableContradictions(); len(durable) > 0 {
		return durable
	}
	if !artifacts.unitIsActive() {
		return nil
	}
	return artifacts.runtimeContradictions()
}

// durableContradictions checks what a VM is made of when nothing is running.
func (artifacts hostArtifacts) durableContradictions() []string {
	files := paths.ForVirtualMachine(artifacts.uuid)
	// The directory is checked first and alone. Every probe below it was skipped
	// when it is gone, so reporting them too would claim we looked inside a tree
	// that is not there.
	if !artifacts.directory {
		return []string{fmt.Sprintf("no VM directory %s", files.Directory())}
	}
	var found []string
	if !artifacts.jail {
		found = append(found, fmt.Sprintf("VM directory has no jail tree %s", files.JailRoot()))
	}
	if !artifacts.environment.complete() {
		found = append(found, fmt.Sprintf(
			"VM directory has no readable %s, so the namespace, tap, veth and address"+
				" this VM owns are unknown", files.NetworkEnvironment(),
		))
	}
	// The disk is the reason this whole package refuses to guess: terminate
	// removes the tree first and the LV last, so a tree standing over a released
	// disk is a crash mid-terminate. Adopting it is how a controller boots a VM
	// whose disk it has already given away.
	if !artifacts.rootVolume {
		found = append(found, fmt.Sprintf("root disk %s is absent", rootVolumeName(artifacts.uuid)))
	}
	return found
}

// runtimeContradictions checks what must be true of a unit that says active.
//
// A unit that is activating or deactivating gets none of these: a host caught
// mid-transition is not a host contradicting itself, and internal/vm already
// reports such a VM as Unknown rather than guessing in either direction.
func (artifacts hostArtifacts) runtimeContradictions() []string {
	environment := artifacts.environment
	var found []string
	switch {
	case !artifacts.namespace:
		found = append(found, fmt.Sprintf(
			"unit is active but its network namespace %s is absent", environment.namespace,
		))
	case !artifacts.tap:
		// Checked only once the namespace exists, because a tap in a namespace
		// that is gone is not a second fault, it is the same one.
		found = append(found, fmt.Sprintf(
			"unit is active but tap %s is absent from namespace %s", environment.tap, environment.namespace,
		))
	}
	if !artifacts.hostVeth {
		found = append(found, fmt.Sprintf(
			"unit is active but its host-side veth %s is absent", environment.hostVeth,
		))
	}
	// The socket is the liveness cross-check: it is Firecracker's own API
	// endpoint, created by the process and gone with it. A unit reporting active
	// over no socket is a unit describing a process that is not there.
	if !artifacts.apiSocket {
		found = append(found, fmt.Sprintf(
			"unit is active but the Firecracker API socket %s is absent",
			paths.ForVirtualMachine(artifacts.uuid).APISocket(),
		))
	}
	return found
}

// survivors is what the host still holds for this UUID. It is what turns a
// quarantine record from "something is missing" into "these things disagree":
// the reason names what was wrong, and this names what is still standing that
// makes the answer ambiguous rather than simply absent.
func (artifacts hostArtifacts) survivors() []string {
	files := paths.ForVirtualMachine(artifacts.uuid)
	var held []string
	if artifacts.directory {
		held = append(held, fmt.Sprintf("directory %s is present", files.Directory()))
	}
	if artifacts.unitLoaded {
		held = append(held, fmt.Sprintf("systemd holds %s (%s/%s)",
			artifacts.unit.Name, artifacts.unit.ActiveState, artifacts.unit.SubState))
	}
	if artifacts.rootVolume {
		held = append(held, fmt.Sprintf("logical volume %s exists", rootVolumeName(artifacts.uuid)))
	}
	if artifacts.dataVolume {
		held = append(held, fmt.Sprintf("logical volume %s exists", dataVolumeName(artifacts.uuid)))
	}
	if artifacts.namespace {
		held = append(held, fmt.Sprintf("network namespace %s is up", artifacts.environment.namespace))
	}
	if artifacts.hostVeth {
		held = append(held, fmt.Sprintf("host-side veth %s is up", artifacts.environment.hostVeth))
	}
	if artifacts.apiSocket {
		held = append(held, fmt.Sprintf("a Firecracker API socket is open at %s", files.APISocket()))
	}
	if artifacts.proxy {
		held = append(held, fmt.Sprintf(
			"the host still answers proxy-NDP for %s", artifacts.environment.address))
	}
	return held
}

// quarantine records one artifact set that could not be read as a VM. The
// evidence leads with what was wrong and closes with what is still standing,
// which is the order an operator reads it in.
func (scanner *Scanner) quarantine(artifacts hostArtifacts, contradictions []string) model.Quarantine {
	return model.Quarantine{
		UUID:     artifacts.uuid,
		Reason:   contradictions[0],
		Evidence: slices.Concat(contradictions, artifacts.survivors()),
		SeenAt:   scanner.clock.Now(),
	}
}

// claims is every artifact name some UUID on this host says it owns, taken from
// the VMs' own sidecars. Names never collide across classes — a namespace name
// is not an IPv6 address — so one set is enough.
type claims map[string]bool

func (claimed claims) add(environment networkEnvironment) {
	for _, name := range []string{environment.namespace, environment.hostVeth, environment.address} {
		if name != "" {
			claimed[name] = true
		}
	}
}

// orphans reports the artifacts no UUID on this host claims.
//
// These are the ones reset-server.py sweeps last and for the same reason: a
// namespace or a proxy-NDP entry whose VM directory is already gone. The NDP
// entries matter most — a host answering neighbour solicitations for a /128 it
// no longer owns silently blackholes that address after the VM has moved
// somewhere else.
//
// A claim is made by a VM's sidecar, so a quarantined VM whose sidecar was
// unreadable claims nothing and its namespace is reported here as well. That is
// two records for one broken VM, which is correct: they are two artifact sets
// and neither one can be read back to the other.
func (scanner *Scanner) orphans(taken inventory, claimed claims) []model.Quarantine {
	var orphaned []model.Quarantine
	for _, name := range taken.directories {
		if !isUUID(name) {
			orphaned = append(orphaned, scanner.orphan(name,
				fmt.Sprintf("%s/%s is not a VM UUID", paths.VirtualMachinesDirectory, name)))
		}
	}
	for _, namespace := range taken.namespaces {
		if isAtlasArtifact(namespace) && !claimed[namespace] {
			orphaned = append(orphaned, scanner.orphan(namespace,
				fmt.Sprintf("network namespace %s is up and no VM on this host claims it", namespace)))
		}
	}
	for _, link := range taken.links {
		if isAtlasArtifact(link) && !claimed[link] {
			orphaned = append(orphaned, scanner.orphan(link,
				fmt.Sprintf("link %s is up and no VM on this host claims it", link)))
		}
	}
	for _, proxy := range taken.proxies {
		if !claimed[proxy.address] {
			orphaned = append(orphaned, scanner.orphan(proxy.address, fmt.Sprintf(
				"the host answers proxy-NDP for %s on %s and no VM on this host claims that address",
				proxy.address, proxy.device)))
		}
	}
	return orphaned
}

// orphan records one unclaimed artifact. model.Quarantine is keyed by UUID and
// these artifacts do not carry one — a namespace is named for the first 12 hex
// digits of a UUID, a veth for 7, a proxy entry for an address — so the field
// carries the only identifier the host retained. Boat does not invert the
// truncation to recover the UUID: a guessed UUID inside a quarantine record is
// exactly the guess this package exists to refuse.
func (scanner *Scanner) orphan(identifier string, reason string) model.Quarantine {
	return model.Quarantine{
		UUID:     identifier,
		Reason:   reason,
		Evidence: []string{reason},
		SeenAt:   scanner.clock.Now(),
	}
}

// isAtlasArtifact reports whether a namespace or link name is one this system
// installs per VM. atlas-park0 is excluded by name: it is the shared dummy every
// sleeping VM's /128 routes out, created by bootstrap and deliberately kept by
// reset-server, so it is host floor and never an orphan.
func isAtlasArtifact(name string) bool {
	if name == parkDevice {
		return false
	}
	return strings.HasPrefix(name, atlasLinkPrefix) || strings.HasPrefix(name, migrationLinkPrefix)
}
