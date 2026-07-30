package units

import (
	"context"
	"fmt"
	"strings"

	"github.com/frappe/boat/internal/model"
)

const (
	// showProperties is what one liveness read asks systemd for.
	//
	// Id is in the list because the answer is a stream of blocks with nothing
	// else to say which unit each one describes. Reading them positionally would
	// work today and would be wrong the first time systemd reorders or omits one.
	//
	// LoadState is what separates "this host does not run this service" from
	// "this service is down" — the distinction that decides whether a unit is
	// reported at all. ActiveState and SubState are the liveness itself.
	showProperties = "--property=Id --property=LoadState --property=ActiveState --property=SubState"

	// loadStateNotFound is systemd's LoadState for a name it holds no unit file
	// for. It is the ONE value that means this host does not run the service;
	// `masked`, `error` and `bad-setting` all mean the host has something and it
	// is not working, which is exactly what supervision is for and is reported.
	loadStateNotFound = "not-found"

	identifierProperty  = "Id"
	loadStateProperty   = "LoadState"
	activeStateProperty = "ActiveState"
	subStateProperty    = "SubState"
)

// read asks systemd about a set of units in one call and keeps the ones this
// host has.
//
// One subprocess for the whole set rather than one per unit: this runs on every
// GET /host and inside every export, and the export is polled per host across
// the fleet. `systemctl show` takes many names, answers a block per name in the
// order asked, and exits 0 even for names it knows nothing about — so the
// missing ones are data in the answer rather than an error to interpret.
func (supervisor *Supervisor) read(
	ctx context.Context, commands commands, names []string,
) ([]model.UnitLiveness, error) {
	if len(names) == 0 {
		return []model.UnitLiveness{}, nil
	}
	template, parameters := showCommand(names)
	output, err := commands.Run(ctx, template, parameters...)
	if err != nil {
		return nil, fmt.Errorf("read the liveness of this host's units: %w", err)
	}
	return parseLiveness(output), nil
}

// showCommand renders one `systemctl show` over every name.
//
// The template grows a `{}` per name rather than interpolating the names into
// it: the literal half stays literal and each name arrives quoted in its own
// hole, so the trust model internal/run is built on holds for a variable-length
// command exactly as it does for a fixed one.
//
// No sudo. This is a property read over the system bus, and the unprivileged
// service user can make it — which is why supervision costs one sudoers grant
// per unit per ACTION and none at all for the read.
func showCommand(names []string) (string, []any) {
	template := "systemctl show " + showProperties + strings.Repeat(" {}", len(names))
	parameters := make([]any, len(names))
	for index, name := range names {
		parameters[index] = name
	}
	return template, parameters
}

// parseLiveness reads `systemctl show`'s blank-line-separated blocks.
//
// The result is empty rather than nil when nothing survives, because the caller
// turns that into an empty array in the export, and an empty array is a claim
// this function is entitled to make: it asked systemd about every supervised
// unit and systemd said this host has none of them.
func parseLiveness(output string) []model.UnitLiveness {
	liveness := []model.UnitLiveness{}
	for _, block := range strings.Split(strings.TrimSpace(output), "\n\n") {
		if unit, present := parseBlock(block); present {
			liveness = append(liveness, unit)
		}
	}
	return liveness
}

// parseBlock turns one `Key=Value` block into a unit, and reports present=false
// for a unit this host does not have.
//
// A unit systemd calls `not-found` is dropped rather than reported inactive.
// Atlas's mirror reads any unit whose active_state is not `active` as down
// (boat_mirror._units_down), so reporting the four supervised names on a host
// that has two of them would flag every host in the fleet as permanently
// degraded — and a health field that is always red is a health field nobody
// reads. "This host does not run atlas-networkd" is not "atlas-networkd is
// down", and only the second is worth waking somebody for.
func parseBlock(block string) (model.UnitLiveness, bool) {
	properties := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		if key, value, found := strings.Cut(strings.TrimSpace(line), "="); found {
			properties[key] = value
		}
	}
	if properties[identifierProperty] == "" || properties[loadStateProperty] == loadStateNotFound {
		return model.UnitLiveness{}, false
	}
	return model.UnitLiveness{
		Name:        properties[identifierProperty],
		ActiveState: properties[activeStateProperty],
		SubState:    properties[subStateProperty],
	}, true
}
