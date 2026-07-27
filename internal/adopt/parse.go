// Parsing: the read half of reset-server.py's _list_* helpers. Each function
// takes one command's stdout and nothing else, so the shape a host actually
// prints is the only thing these have to get right.

package adopt

import (
	"slices"
	"strconv"
	"strings"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/paths"
)

// parseListing reads `ls -1`: one name per line.
func parseListing(output string) []string {
	var names []string
	for line := range strings.Lines(output) {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// parseUnits reads `systemctl list-units --all --no-legend --plain`. --plain
// drops the tree glyphs and --no-legend the header/footer, so every row is
// `<unit> <load> <active> <sub> <description>` and the first three columns are
// the whole of a unit's liveness.
func parseUnits(output string) []model.UnitLiveness {
	var units []model.UnitLiveness
	for line := range strings.Lines(output) {
		fields := strings.Fields(line)
		if len(fields) < 4 || !strings.HasSuffix(fields[0], unitNameSuffix) {
			continue
		}
		// A not-found instance is a name systemd is holding open for something
		// that references it, not a unit file this host has. Reading it as a VM
		// would quarantine a UUID no artifact on the host actually carries.
		if fields[1] == unitLoadNotFound {
			continue
		}
		units = append(units, model.UnitLiveness{Name: fields[0], ActiveState: fields[2], SubState: fields[3]})
	}
	return units
}

// parseNamespaces reads `ip netns list`, whose lines are "<name>" or
// "<name> (id: N)".
func parseNamespaces(output string) []string {
	var namespaces []string
	for line := range strings.Lines(output) {
		if fields := strings.Fields(line); len(fields) > 0 {
			namespaces = append(namespaces, fields[0])
		}
	}
	return namespaces
}

// parseLinks reads `ip -o link show`, whose lines are "<index>: <name>@<peer>:".
// Every link is kept, not just this system's: attribution is by exact name from
// a VM's network.env, and a prefix filter here would decide what counts as ours
// twice.
func parseLinks(output string) []string {
	var links []string
	for line := range strings.Lines(output) {
		parts := strings.SplitN(line, ": ", 3)
		if len(parts) < 2 {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimSpace(parts[1]), "@")
		if name != "" {
			links = append(links, name)
		}
	}
	return links
}

// parseProxies reads `ip -6 neigh show proxy`, whose lines are
// "<address> dev <device> proxy".
func parseProxies(output string) []neighbourProxy {
	var proxies []neighbourProxy
	for line := range strings.Lines(output) {
		fields := strings.Fields(line)
		device := slices.Index(fields, "dev")
		if device < 0 || device+1 >= len(fields) {
			continue
		}
		proxies = append(proxies, neighbourProxy{address: fields[0], device: fields[device+1]})
	}
	return proxies
}

// parseVolumes reads `lvs --noheadings --separator ,`. The separator is explicit
// because pool_lv and origin are empty for a plain LV, and empty columns in the
// default whitespace-aligned output cannot be told apart from missing ones.
func parseVolumes(output string) []model.LogicalVolume {
	var volumes []model.LogicalVolume
	for line := range strings.Lines(output) {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) < 4 || strings.TrimSpace(fields[0]) == "" {
			continue
		}
		// An unparseable size is reported as zero rather than failing the scan.
		// The coherence rules only ever ask whether a volume EXISTS; SizeBytes is
		// inventory colour, and no decision in this package reads it.
		size, _ := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
		volumes = append(volumes, model.LogicalVolume{
			Name:      strings.TrimSpace(fields[0]),
			SizeBytes: size,
			Pool:      strings.TrimSpace(fields[2]),
			Origin:    strings.TrimSpace(fields[3]),
		})
	}
	return volumes
}

// isUUID reports whether name has the 8-4-4-4-12 hex shape Atlas gives every VM.
// A directory, unit instance or LV suffix that is not one names something else,
// and something else is not a VM whatever it looks like.
//
// Delegated so there is one definition of the shape in the repo. It is also the
// definition that keeps a name from becoming a path: a UUID is spliced into
// every command run against a VM and matched by a `*` in the sudo allow-list,
// where `*` covers `/`, `.` and spaces.
//
// Note it is lowercase-only, deliberately. Atlas mints lowercase, and accepting
// uppercase would make one VM answer to two names — two directories, two sets of
// artifacts, and an adoption scan that reports it twice.
func isUUID(name string) bool { return paths.IsUUID(name) }
