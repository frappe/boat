package adopt

import (
	"context"
	"strings"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/paths"
)

// The network.env keys this package reads. provision writes the sidecar and the
// vm-network-up/down hooks read it back, which is what lets a host rebuild a
// VM's networking after a reboot without reaching into a database. Boat reads it
// for the same reason: it is the host's own record of which namespace, tap, veth
// and address belong to this UUID.
const (
	namespaceKey = "ATLAS_NETNS"
	tapKey       = "TAP_DEVICE"
	hostVethKey  = "HOST_VETH"
	addressKey   = "VIRTUAL_MACHINE_IPV6"
)

// unitActive is the ActiveState of a unit claiming a live Firecracker. It is the
// same string internal/vm reads, and the only unit state the coherence rules
// treat as a claim about the host.
const unitActive = "active"

// networkEnvironment is the slice of a VM's network.env this package needs: the
// names of the runtime artifacts that VM owns.
//
// Boat does not re-derive these from the UUID even though Atlas derives them
// that way. A derivation that drifts from the writer mis-attributes silently,
// and mis-attribution here means adopting a VM holding another VM's tap.
type networkEnvironment struct {
	namespace string
	tap       string
	hostVeth  string
	address   string
}

// complete reports whether the sidecar named everything a VM's networking is
// made of. A VM directory whose sidecar is absent or half-written is a tree
// terminate had already started on: the hook that tears the networking down
// reads this same file, and it cannot run without it.
func (environment networkEnvironment) complete() bool {
	return environment.namespace != "" && environment.tap != "" &&
		environment.hostVeth != "" && environment.address != ""
}

// hostArtifacts is everything one UUID left on this host. Each field is an
// observation, never a conclusion: what they add up to is coherence.go's job.
type hostArtifacts struct {
	uuid        string
	directory   bool
	jail        bool
	environment networkEnvironment
	unit        model.UnitLiveness
	unitLoaded  bool
	rootVolume  bool
	dataVolume  bool
	namespace   bool
	tap         bool
	hostVeth    bool
	proxy       bool
	apiSocket   bool
}

func (artifacts hostArtifacts) unitIsActive() bool {
	return artifacts.unitLoaded && artifacts.unit.ActiveState == unitActive
}

func examineAll(ctx context.Context, commands commands, taken inventory) []hostArtifacts {
	var examined []hostArtifacts
	for _, uuid := range taken.candidates() {
		examined = append(examined, examine(ctx, commands, taken, uuid))
	}
	return examined
}

// examine collects one UUID's artifacts from the inventory and from the probes
// the inventory cannot answer.
func examine(ctx context.Context, commands commands, taken inventory, uuid string) hostArtifacts {
	artifacts := hostArtifacts{
		uuid:       uuid,
		directory:  taken.hasDirectory(uuid),
		rootVolume: taken.hasVolume(rootVolumeName(uuid)),
		dataVolume: taken.hasVolume(dataVolumeName(uuid)),
	}
	artifacts.unit, artifacts.unitLoaded = taken.unitFor(uuid)
	if artifacts.directory {
		examineDisk(ctx, commands, uuid, &artifacts)
	}
	examineNetwork(ctx, commands, taken, &artifacts)
	return artifacts
}

// examineDisk reads the VM tree. The probes are skipped when the directory is
// gone, so a missing jail is only ever reported for a UUID whose tree we
// actually looked inside — "we did not look" and "it is not there" are different
// answers and quarantine evidence has to say which one it is.
//
// Every probe goes through sudo: the VM tree is 0700 owned by the per-VM uid, so
// an in-process stat would report "absent" for a file that is plainly there.
func examineDisk(ctx context.Context, commands commands, uuid string, artifacts *hostArtifacts) {
	files := paths.ForVirtualMachine(uuid)
	artifacts.jail = commands.OK(ctx, "sudo test -d {}", files.JailRoot())
	artifacts.apiSocket = commands.OK(ctx, "sudo test -S {}", files.APISocket())
	artifacts.environment = parseNetworkEnvironment(readNetworkEnvironment(ctx, commands, files))
}

// readNetworkEnvironment returns the sidecar's text, or empty when there is
// none. The read is unchecked because an absent sidecar is evidence rather than
// a failure — terminate removes it before the unit's ExecStopPost runs, so a
// missing one is a normal thing to find on a host mid-teardown. The backstop is
// that empty text parses to an incomplete environment, which is a contradiction,
// which quarantines: nothing is silently ingested because a read came back
// blank.
func readNetworkEnvironment(ctx context.Context, commands commands, files paths.VirtualMachine) string {
	text, err := commands.RunUnchecked(ctx, "sudo cat {}", files.NetworkEnvironment())
	if err != nil {
		return ""
	}
	return text
}

// examineNetwork cross-checks the runtime artifacts the sidecar named against
// the ones the host is actually carrying. The tap is the one artifact no
// host-wide enumeration can see: it lives inside the VM's namespace, so it is
// asked for there.
func examineNetwork(ctx context.Context, commands commands, taken inventory, artifacts *hostArtifacts) {
	environment := artifacts.environment
	artifacts.namespace = taken.hasNamespace(environment.namespace)
	artifacts.hostVeth = taken.hasLink(environment.hostVeth)
	artifacts.proxy = taken.hasProxy(environment.address)
	if artifacts.namespace && environment.tap != "" {
		artifacts.tap = commands.OK(
			ctx, "sudo ip netns exec {} ip link show {}", environment.namespace, environment.tap,
		)
	}
}

// parseNetworkEnvironment reads the shell KEY=value sidecar the way sourcing it
// would. Blank lines and comments are skipped and surrounding quotes stripped:
// provision writes bare values, but a reader that only handles its own writer's
// output is a reader that fails on the first hand-edited host.
func parseNetworkEnvironment(text string) networkEnvironment {
	values := map[string]string{}
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key, value, found := strings.Cut(line, "="); found {
			values[strings.TrimSpace(key)] = unquote(strings.TrimSpace(value))
		}
	}
	return networkEnvironment{
		namespace: values[namespaceKey],
		tap:       values[tapKey],
		hostVeth:  values[hostVethKey],
		address:   values[addressKey],
	}
}

func unquote(value string) string {
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		return value[1 : len(value)-1]
	}
	return value
}
