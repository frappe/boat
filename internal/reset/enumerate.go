package reset

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/frappe/boat/internal/paths"
)

// The host enumeration: read-only pokes, each tolerating absence (RunUnchecked
// discards the exit code, matching the Python's check=False). The parsing that bit
// Atlas on real hosts — the lsblk padding, the ip -o link `name@peer` split, the nft
// JSON shape — is isolated here so it is unit-testable with no host, the same
// discipline lvm.py drew.

// sweepParkState deletes every wake_ SYN-trap forward rule and named counter (the
// park state). Rules first, so the counters they reference can then be dropped.
func sweepParkState(ctx context.Context, cmd commands) {
	chain, _ := cmd.RunUnchecked(ctx, "sudo nft -a list chain inet atlas forward")
	for _, line := range strings.Split(chain, "\n") {
		if !strings.Contains(line, "wake_") || !strings.Contains(line, "handle") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		// `nft -a` prints the handle as the last token of the rule line.
		cmd.RunUnchecked(ctx, "sudo nft delete rule inet atlas forward handle {}", fields[len(fields)-1])
	}
	for _, name := range listWakeCounters(ctx, cmd) {
		cmd.RunUnchecked(ctx, "sudo nft delete counter inet atlas {}", name)
	}
}

// listVMDirectories lists the VM UUID directories. It admits only real UUIDs: these
// names are learned from a directory listing, not from Atlas, and each is spliced
// into `firecracker-vm@<name>.service` and `rm -rf <dir>/<name>` — a `..` there
// would rm the parent — so a non-UUID entry is skipped here (the final directory
// sweep still removes it). This is the hardening paths.IsUUID exists for, stronger
// than the Python, which trusted the listing.
func listVMDirectories(ctx context.Context, cmd commands) []string {
	output, _ := cmd.RunUnchecked(ctx, "ls -1 {}", paths.VirtualMachinesDirectory)
	var uuids []string
	for _, line := range trimmedLines(output) {
		if paths.IsUUID(line) {
			uuids = append(uuids, line)
		}
	}
	return uuids
}

// listUnits lists loaded systemd units matching a glob. --plain drops the tree
// glyphs; --no-legend drops the header/footer, so each row is `<unit> <load> …` and
// token 0 is the unit. Only .service rows are kept.
func listUnits(ctx context.Context, cmd commands, pattern string) []string {
	output, _ := cmd.RunUnchecked(ctx, "systemctl list-units {} --all --no-legend --plain", pattern)
	var units []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		token := strings.Fields(line)[0]
		if strings.HasSuffix(token, ".service") {
			units = append(units, token)
		}
	}
	return units
}

// listNetNS lists network namespaces. Each line is "<name>" or "<name> (id: N)"; the
// first token is the name.
func listNetNS(ctx context.Context, cmd commands) []string {
	output, _ := cmd.RunUnchecked(ctx, "ip netns list")
	var names []string
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		names = append(names, strings.Fields(line)[0])
	}
	return names
}

func isAtlasNamespace(name string) bool { return strings.HasPrefix(name, "atlas-") }

// listAtlasLinks lists the host-side veth/tap/mig6 links. A `ip -o link show` line
// reads "<idx>: <name>@peer: <flags>"; the device name is before the `@`.
func listAtlasLinks(ctx context.Context, cmd commands) []string {
	output, _ := cmd.RunUnchecked(ctx, "ip -o link show")
	var links []string
	for _, line := range strings.Split(output, "\n") {
		parts := strings.SplitN(line, ": ", 3)
		if len(parts) < 2 {
			continue
		}
		name := strings.TrimSpace(strings.SplitN(parts[1], "@", 2)[0])
		if strings.HasPrefix(name, "veth-") || strings.HasPrefix(name, "tap") || strings.HasPrefix(name, "mig6-") {
			links = append(links, name)
		}
	}
	return links
}

// ndpEntry is one proxy-NDP entry: the /128 and the device it is proxied on.
type ndpEntry struct {
	address string
	device  string
}

// listNDPProxy lists proxy-NDP entries. `ip -6 neigh show proxy` lines read
// "<addr> dev <dev> proxy"; the address is token 0 and the device follows `dev`.
func listNDPProxy(ctx context.Context, cmd commands) []ndpEntry {
	output, _ := cmd.RunUnchecked(ctx, "ip -6 neigh show proxy")
	var entries []ndpEntry
	for _, line := range strings.Split(output, "\n") {
		tokens := strings.Fields(line)
		for index, token := range tokens {
			if token == "dev" && index+1 < len(tokens) {
				entries = append(entries, ndpEntry{address: tokens[0], device: tokens[index+1]})
				break
			}
		}
	}
	return entries
}

// listWakeCounters lists the named wake_ counters. `nft -j` reports counters as
// `{"counter":{"name":…}}` elements alongside a metainfo element; a parse failure
// (no table) yields none.
func listWakeCounters(ctx context.Context, cmd commands) []string {
	output, _ := cmd.RunUnchecked(ctx, "sudo nft -j list counters table inet atlas")
	var document struct {
		NFTables []map[string]json.RawMessage `json:"nftables"`
	}
	if json.Unmarshal([]byte(output), &document) != nil {
		return nil
	}
	var names []string
	for _, item := range document.NFTables {
		raw, ok := item["counter"]
		if !ok {
			continue
		}
		var counter struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &counter) == nil && strings.HasPrefix(counter.Name, "wake_") {
			names = append(names, counter.Name)
		}
	}
	return names
}

// listBoundNBD lists the bound /dev/nbdN clients: a bound device reports a non-zero
// size in /sys/block/nbdN/size.
func listBoundNBD(ctx context.Context, cmd commands) []string {
	output, _ := cmd.RunUnchecked(ctx, "ls -1 /sys/block")
	var devices []string
	for _, name := range strings.Split(output, "\n") {
		name = strings.TrimSpace(name)
		if !strings.HasPrefix(name, "nbd") {
			continue
		}
		size, _ := cmd.RunUnchecked(ctx, "cat {}", "/sys/block/"+name+"/size")
		size = strings.TrimSpace(size)
		if size != "" && size != "0" {
			devices = append(devices, "/dev/"+name)
		}
	}
	return devices
}

// listDMTargets lists the live dm-clone targets (`dmsetup ls --target clone`). Atlas
// installs none at bootstrap, so on a just-bootstrapped host this is empty.
func listDMTargets(ctx context.Context, cmd commands) []string {
	output, _ := cmd.RunUnchecked(ctx, "sudo dmsetup ls --target clone")
	var targets []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "No devices found" {
			continue
		}
		targets = append(targets, strings.Fields(line)[0])
	}
	return targets
}

// listAtlasLVs lists every LV name in the atlas VG (pool0 among them; the caller
// skips it).
func listAtlasLVs(ctx context.Context, cmd commands) []string {
	output, _ := cmd.RunUnchecked(ctx, "sudo lvs --noheadings -o lv_name {}", volumeGroup)
	return trimmedLines(output)
}

// trimmedLines returns the non-empty lines of output, each stripped of surrounding
// whitespace (lvs pads its --noheadings output with two leading spaces).
func trimmedLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}
