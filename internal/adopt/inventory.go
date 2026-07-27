package adopt

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/paths"
)

const (
	// volumeGroup is Atlas's VG name (scripts/lib/atlas/lvm.py's ThinPool
	// default). Inherited, not chosen: Boat adopts hosts whose disks already
	// live in it.
	volumeGroup = "atlas"

	// virtualMachineUnitPattern matches every per-VM instance and nothing else,
	// so this enumeration can never sweep in sshd or boat.service itself.
	virtualMachineUnitPattern = "firecracker-vm@*"

	unitNamePrefix = "firecracker-vm@"
	unitNameSuffix = ".service"

	// Atlas's per-VM disk names. Snapshot LVs (atlas-snap-, atlas-datasnap-) are
	// keyed by a SNAPSHOT uuid rather than a VM's, and base images
	// (atlas-image-) by a name, so neither identifies a VM — reading them as one
	// would quarantine every snapshot on the host as a disk with no directory.
	rootVolumePrefix = "atlas-vm-"
	dataVolumePrefix = "atlas-data-"

	// atlasLinkPrefix and migrationLinkPrefix are the host-side link names this
	// system installs: atlas-h<hex> is a VM's host-side veth and mig6-<hex> a
	// migration carrier's tun device.
	atlasLinkPrefix     = "atlas-"
	migrationLinkPrefix = "mig6-"

	// parkDevice is the shared always-up dummy every SLEEPING VM's /128 routes
	// out (scripts/lib/atlas/park.py). Bootstrap creates it and reset-server
	// deliberately keeps it, so it is host floor and never an orphan.
	parkDevice = "atlas-park0"

	// unitLoadNotFound is systemd's LOAD column for an instance with no unit
	// file. Such a row is a name systemd is holding, not a VM.
	unitLoadNotFound = "not-found"
)

// neighbourProxy is one proxy-NDP entry: the host answering neighbour
// solicitations for an address on a device. Atlas installs these only per VM,
// which is what makes an unclaimed one evidence rather than noise.
type neighbourProxy struct {
	address string
	device  string
}

// inventory is one pass over every artifact class reset-server.py knows how to
// destroy, read instead of executed.
type inventory struct {
	directories []string
	units       []model.UnitLiveness
	namespaces  []string
	links       []string
	proxies     []neighbourProxy
	volumes     []model.LogicalVolume
}

// takeInventory runs the six enumerations.
//
// The reads are threaded through one error-accumulating helper so the list of
// what a host holds reads as a list. A failed enumeration aborts the scan: an
// inventory missing a class silently reports a host holding less than it holds.
func takeInventory(ctx context.Context, commands commands) (inventory, error) {
	reading := &enumeration{ctx: ctx, commands: commands}
	taken := inventory{
		directories: parseListing(reading.read("ls -1 {}", paths.VirtualMachinesDirectory)),
		units: parseUnits(reading.read(
			"systemctl list-units {} --all --no-legend --plain", virtualMachineUnitPattern,
		)),
		namespaces: parseNamespaces(reading.read("ip netns list")),
		links:      parseLinks(reading.read("ip -o link show")),
		proxies:    parseProxies(reading.read("ip -6 neigh show proxy")),
		volumes: parseVolumes(reading.read(
			"sudo lvs --noheadings --nosuffix --units b --separator , "+
				"-o lv_name,lv_size,pool_lv,origin {}", volumeGroup,
		)),
	}
	if reading.err != nil {
		return inventory{}, fmt.Errorf("scan host: %w", reading.err)
	}
	return taken, nil
}

// enumeration is the six reads plus the first error any of them returned.
type enumeration struct {
	ctx      context.Context
	commands commands
	err      error
}

// read returns empty output once something has failed, so the remaining
// enumerations are skipped and the caller discards the whole inventory.
func (enumeration *enumeration) read(template string, parameters ...any) string {
	if enumeration.err != nil {
		return ""
	}
	output, err := enumeration.commands.Run(enumeration.ctx, template, parameters...)
	enumeration.err = err
	return output
}

// candidates is every UUID any artifact class names, sorted.
//
// Only the three classes that carry a whole UUID contribute one. A namespace is
// named for the first 12 hex digits of a UUID and a tap for the first 9, so
// neither can be inverted back to the VM it belongs to; they are attributed to a
// candidate through the VM's own network.env, never used to invent one.
func (taken inventory) candidates() []string {
	found := map[string]bool{}
	for _, name := range taken.directories {
		addCandidate(found, name)
	}
	for _, unit := range taken.units {
		addCandidate(found, unitUUID(unit.Name))
	}
	for _, volume := range taken.volumes {
		addCandidate(found, volumeUUID(volume.Name))
	}
	return slices.Sorted(maps.Keys(found))
}

func addCandidate(found map[string]bool, uuid string) {
	if isUUID(uuid) {
		found[uuid] = true
	}
}

// unitFor returns the systemd instance holding this UUID, if systemd holds one
// at all. A stopped VM's instance may have been unloaded and is then absent,
// which is why a missing unit is never on its own evidence of anything.
func (taken inventory) unitFor(uuid string) (model.UnitLiveness, bool) {
	name := paths.ForVirtualMachine(uuid).SystemdUnit()
	for _, unit := range taken.units {
		if unit.Name == name {
			return unit, true
		}
	}
	return model.UnitLiveness{}, false
}

// The membership queries below all refuse an empty name: a sidecar that never
// set a key must not match an entry a parser never emitted.
func (taken inventory) hasDirectory(uuid string) bool {
	return uuid != "" && slices.Contains(taken.directories, uuid)
}

func (taken inventory) hasVolume(name string) bool {
	return slices.ContainsFunc(taken.volumes, func(volume model.LogicalVolume) bool {
		return volume.Name == name
	})
}

func (taken inventory) hasProxy(address string) bool {
	return address != "" && slices.ContainsFunc(taken.proxies, func(proxy neighbourProxy) bool {
		return proxy.address == address
	})
}

func (taken inventory) hasNamespace(name string) bool {
	return name != "" && slices.Contains(taken.namespaces, name)
}

func (taken inventory) hasLink(name string) bool {
	return name != "" && slices.Contains(taken.links, name)
}

func unitUUID(name string) string {
	return strings.TrimSuffix(strings.TrimPrefix(name, unitNamePrefix), unitNameSuffix)
}

func volumeUUID(name string) string {
	for _, prefix := range []string{rootVolumePrefix, dataVolumePrefix} {
		if uuid, found := strings.CutPrefix(name, prefix); found {
			return uuid
		}
	}
	return ""
}

func rootVolumeName(uuid string) string { return rootVolumePrefix + uuid }

func dataVolumeName(uuid string) string { return dataVolumePrefix + uuid }
