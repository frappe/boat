package adopt

import (
	"context"
	"fmt"
	"slices"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/sidecar"
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
	// firecracker is whether a live Firecracker ANSWERED for this UUID, not
	// whether its socket file exists. See examineDisk.
	firecracker bool
}

func (artifacts hostArtifacts) unitIsActive() bool {
	return artifacts.unitLoaded && artifacts.unit.ActiveState == unitActive
}

// examineAll collects every candidate UUID's artifacts, and fails the whole scan
// the moment one of them cannot be read. That is this package's rule rather than
// this function's: a probe that failed and was skipped would leave a VM looking
// like it holds one artifact fewer than it does, and the record of that is a
// quarantine blaming the host for a fault of ours.
func (scanner *Scanner) examineAll(
	ctx context.Context, commands commands, runner *run.Runner, taken inventory,
) ([]hostArtifacts, error) {
	var examined []hostArtifacts
	for _, uuid := range taken.candidates() {
		artifacts, err := scanner.examine(ctx, commands, runner, taken, uuid)
		if err != nil {
			return nil, err
		}
		examined = append(examined, artifacts)
	}
	return examined, nil
}

// examine collects one UUID's artifacts from the inventory and from the probes
// the inventory cannot answer.
func (scanner *Scanner) examine(
	ctx context.Context, commands commands, runner *run.Runner, taken inventory, uuid string,
) (hostArtifacts, error) {
	artifacts := hostArtifacts{
		uuid:       uuid,
		directory:  taken.hasDirectory(uuid),
		rootVolume: taken.hasVolume(rootVolumeName(uuid)),
		dataVolume: taken.hasVolume(dataVolumeName(uuid)),
	}
	artifacts.unit, artifacts.unitLoaded = taken.unitFor(uuid)
	if artifacts.directory {
		if err := scanner.examineDisk(ctx, commands, runner, uuid, &artifacts); err != nil {
			return hostArtifacts{}, err
		}
	}
	if err := examineNetwork(ctx, commands, taken, &artifacts); err != nil {
		return hostArtifacts{}, err
	}
	return artifacts, nil
}

// examineDisk reads the VM tree. The probes are skipped when the directory is
// gone, so a missing jail is only ever reported for a UUID whose tree we
// actually looked inside — "we did not look" and "it is not there" are different
// answers and quarantine evidence has to say which one it is.
//
// Every probe goes through sudo: the VM tree is 0700 owned by the per-VM uid, so
// an in-process stat would report "absent" for a file that is plainly there.
//
// The Firecracker question is asked of the Firecracker. This used to be
// `test -S` on the API socket, which answers a weaker question than the one the
// coherence rules want: a unix socket inode outlives the process that bound it,
// so a Firecracker that segfaulted leaves a socket behind that stat is perfectly
// happy with — and a scan that read that as a running VM would adopt a dead one
// as healthy and report it to Atlas as Running. internal/fcattach talks to the
// far end instead, which is the test it was written for.
//
// BOTH probes obey the same rule, and until run.Probe existed only one of them
// did: a probe that could not be MADE is an error and fails the scan, while "it
// is not there" and "nothing answered" are answers and are not. The jail check
// was a bool, so a denied `test -d` recorded a jail tree as missing — and a
// directory with no jail is a half-removed tree, which quarantines. One absent
// allow-list line would have quarantined every VM on the host, with evidence
// blaming the host for it.
func (scanner *Scanner) examineDisk(
	ctx context.Context, commands commands, runner *run.Runner, uuid string, artifacts *hostArtifacts,
) error {
	files := paths.ForVirtualMachine(uuid)
	jail, err := commands.Probe(ctx, "sudo test -d {}", files.JailRoot())
	if err != nil {
		return fmt.Errorf("could not tell whether %s has a jail tree: %w", uuid, err)
	}
	artifacts.jail = jail == run.Yes
	artifacts.environment = parseNetworkEnvironment(readNetworkEnvironment(ctx, commands, files))
	_, live, err := scanner.liveness(ctx, runner, uuid)
	if err != nil {
		return fmt.Errorf("could not tell whether a Firecracker is running for %s: %w", uuid, err)
	}
	artifacts.firecracker = live
	return nil
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
//
// And because that one is a command rather than a lookup in an inventory already
// taken, it is the one that can fail — which is why this returns an error at all.
// A tap read as missing is an active VM with no tap, which is a contradiction and
// quarantines it; doing that to a healthy VM because the namespace could not be
// entered would hide it from Atlas over a fault of ours.
//
// The namespace's links are LISTED and the tap looked for among them, rather
// than named to `ip link show` and the exit code read. Measured on a host:
//
//	ip -o link show <a device that is not there>   exit 1, `Device "…" does not exist.`
//
// which is the ordinary answer for a VM whose tap is genuinely gone, and is
// shaped exactly like a denied sudo — same exit code, both complaining on
// stderr. Nothing separates them, so the question is asked in the form that has
// no ambiguity to separate: the listing exits zero, an absent tap is a name that
// is not in it, and every non-zero exit is a fault. A namespace that vanished
// between `ip netns list` and here answers 255 and fails the scan, which is the
// honest reading of a host that changed under us mid-scan.
func examineNetwork(
	ctx context.Context, commands commands, taken inventory, artifacts *hostArtifacts,
) error {
	environment := artifacts.environment
	artifacts.namespace = taken.hasNamespace(environment.namespace)
	artifacts.hostVeth = taken.hasLink(environment.hostVeth)
	artifacts.proxy = taken.hasProxy(environment.address)
	if !artifacts.namespace || environment.tap == "" {
		return nil
	}
	listing, err := commands.Run(ctx, "sudo ip -n {} -o link show", environment.namespace)
	if err != nil {
		return fmt.Errorf("could not read the links in namespace %s: %w", environment.namespace, err)
	}
	artifacts.tap = slices.Contains(parseLinks(listing), environment.tap)
	return nil
}

// parseNetworkEnvironment names the four artifacts this package cross-checks
// against the host. The reading is internal/sidecar's; what the keys mean is
// this package's, which is why the mapping stays here.
func parseNetworkEnvironment(text string) networkEnvironment {
	values := sidecar.Parse(text)
	return networkEnvironment{
		namespace: values[namespaceKey],
		tap:       values[tapKey],
		hostVeth:  values[hostVethKey],
		address:   values[addressKey],
	}
}
