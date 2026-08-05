package park

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/run"
)

// The two per-minute questions the controller asks a host about sleepy VMs
// (spec/32): "has anything talked to these Running VMs since I last asked?" and
// "have any of these Sleeping VMs been woken behind my back?". Ports of
// scripts/poll-vm-traffic.py and scripts/probe-woken-vms.py.
//
// Both are READ-ONLY and both answer in booleans, which is the whole design.
// Atlas runs them as probes rather than Tasks — no row per poll — and it never
// sees a byte count or a marker path, only the decision. Raw counters are
// host-local, ephemeral, and not database state; the delta is computed here, on
// the host that owns the counter.
//
// These are NOT the wake counters counters.go reads, and the difference is worth
// stating because the two live in the same nft table. The wake trap's counter is a
// NAMED table-scope counter (`wake_<hex>`, packets) that exists only while a VM is
// PARKED; the idle poll reads the inline `counter` on the per-VM forward accepts
// the bring-up installs, which exist only while a VM is RUNNING — and an unpark
// deletes the first as it rebuilds the second. One reader could not serve both:
// asking `nft list counters` about a Running VM answers with nothing at all, which
// reads as a perfectly idle VM and puts a busy guest to sleep.

// TrafficTarget is one VM the controller wants an activity answer for: its UUID,
// and the public /128 whose forward rules carry the counter.
type TrafficTarget struct {
	UUID    string
	Address string
}

// ParseTrafficTargets reads the `--vms-json` argument poll-vm-traffic takes:
// `[{"name": "<uuid>", "ipv6_address": "<v6>"}]`. One JSON argument rather than
// repeated flags because the pair travels together — a UUID without its address
// names a counter nothing can be read from.
func ParseTrafficTargets(document string) ([]TrafficTarget, error) {
	var listed []struct {
		Name    string `json:"name"`
		Address string `json:"ipv6_address"`
	}
	if err := json.Unmarshal([]byte(document), &listed); err != nil {
		return nil, fmt.Errorf("vms-json: %w", err)
	}
	targets := make([]TrafficTarget, 0, len(listed))
	for _, entry := range listed {
		targets = append(targets, TrafficTarget{UUID: entry.Name, Address: entry.Address})
	}
	return targets, nil
}

// PollTraffic reports, per VM UUID, whether its traffic counter MOVED since the
// last poll — true meaning active, false meaning idle and eligible to be slept.
func PollTraffic(
	ctx context.Context, runner *run.Runner, targets []TrafficTarget,
) (map[string]bool, error) {
	return newParker(runner).pollTraffic(ctx, targets)
}

func (parker *parker) pollTraffic(ctx context.Context, targets []TrafficTarget) (map[string]bool, error) {
	active := map[string]bool{}
	if len(targets) == 0 {
		return active, nil
	}
	for _, target := range targets {
		// The UUID becomes the path of the counter file this poll writes, so it is
		// held to the shape every VM on a host is named by before it is rendered.
		if !paths.IsUUID(target.UUID) {
			return nil, fmt.Errorf("poll-vm-traffic: %q is not a VM UUID", target.UUID)
		}
	}
	// No forward chain means the host rebooted before any VM unit started, so every
	// counter is gone. Every VM answers ACTIVE rather than idle: a chain that is not
	// there is an anomaly about the host, and sleeping a fleet on it would be acting
	// on the absence of evidence.
	if !parker.commands.OK(ctx, "sudo nft list chain inet atlas {}", forwardChain) {
		for _, target := range targets {
			active[target.UUID] = true
		}
		return active, nil
	}
	// One listing for the whole host, read CHECKED. The chain was just proven to be
	// there, so a failure now is a fault rather than an answer, and reading it as an
	// empty chain would report every VM's byte total as zero — which is a counter
	// that never moves, which is a fleet that all goes to sleep.
	listing, err := parker.commands.Run(ctx, "sudo nft list chain inet atlas {}", forwardChain)
	if err != nil {
		return nil, err
	}
	for _, target := range targets {
		active[target.UUID] = parker.trafficMoved(target, listing)
	}
	return active, nil
}

// trafficMoved compares this VM's current byte total to the one the last poll
// recorded, and records the new one.
//
// Every anomaly answers ACTIVE, and that direction is the whole judgement here. A
// VM wrongly called active stays awake and costs RAM until the next poll; a VM
// wrongly called idle is STOPPED, and a guest stopped because a counter was
// unreadable is an outage nobody can attribute. So: no baseline is active, and a
// counter that went backwards is active.
func (parker *parker) trafficMoved(target TrafficTarget, listing string) bool {
	counterFile := parker.filesFor(target.UUID).trafficCounter
	current := trafficBytes(listing, target.Address)
	last, seen := lastTraffic(counterFile)
	recordTraffic(counterFile, current)
	if !seen {
		// First poll: nothing to compare against, and a VM this host has never
		// observed must not be slept on that silence.
		return true
	}
	if current < last {
		// A counter does not count down. The chain was flushed or the host rebooted
		// and the rule was re-created at zero, so a reset is never read as idleness.
		return true
	}
	return current > last
}

// trafficBytes sums the byte totals of every forward rule that mentions this VM's
// address.
//
// SUMMING rather than taking the first, and it is a real host state rather than
// caution: two starts without an intervening chain flush leave two accept rules
// for the same VM, and reading one of them reports a VM whose traffic all landed
// on the other as idle. (The bring-up's guard now prevents the duplicate — see
// vmnetwork.forwardRules, where matching nft's quoted listing was the fix — but a
// host that predates it still carries the pair.)
func trafficBytes(listing string, address string) int64 {
	var total int64
	for line := range strings.Lines(listing) {
		if !strings.Contains(line, address) || !strings.Contains(line, "counter") {
			continue
		}
		if bytes, found := bytesInRule(line); found {
			total += bytes
		}
	}
	return total
}

// bytesInRule reads the `bytes N` total out of one nft rule line. Fields rather
// than a pattern: nft prints `counter packets 12 bytes 3456`, so the number is the
// token after `bytes`, and whole-token comparison is the word boundary the
// Python's `\bbytes` was asking for.
func bytesInRule(line string) (int64, bool) {
	fields := strings.Fields(line)
	for index := 0; index+1 < len(fields); index++ {
		if fields[index] != "bytes" {
			continue
		}
		if total, err := strconv.ParseInt(fields[index+1], 10, 64); err == nil {
			return total, true
		}
	}
	return 0, false
}

// lastTraffic reads the total the previous poll recorded.
//
// Read in-process rather than through the command seam because this file is the
// POLL's own scratch space rather than a fact about the VM: it is written by the
// same verb that reads it, one minute earlier, and it exists so that the
// controller never has to hold a byte count. Every failure — no file at all on the
// first poll, a truncated write, a value that is not a number — answers "no
// baseline", which the caller reads as active.
func lastTraffic(counterFile string) (int64, bool) {
	content, err := os.ReadFile(counterFile)
	if err != nil {
		return 0, false
	}
	var record struct {
		Bytes *int64 `json:"bytes"`
	}
	if json.Unmarshal(content, &record) != nil || record.Bytes == nil {
		return 0, false
	}
	return *record.Bytes, true
}

// recordTraffic stores the total for the next poll to compare against.
//
// Best-effort, and the backstop is the poll itself: a total that could not be
// written leaves the older one in place, so the next tick compares against a stale
// baseline and reports ACTIVE for anything that moved in between — never idle for
// a VM that was busy. Failing the verb instead would let one unwritable file stop
// every VM on the host from being observed.
func recordTraffic(counterFile string, total int64) {
	_ = os.WriteFile(counterFile, []byte(fmt.Sprintf("{\"bytes\": %d}", total)), 0o644)
}

// ParseUUIDs reads the `--vms-json` argument probe-woken-vms takes:
// `["<uuid>", …]`.
func ParseUUIDs(document string) ([]string, error) {
	var uuids []string
	if err := json.Unmarshal([]byte(document), &uuids); err != nil {
		return nil, fmt.Errorf("vms-json: %w", err)
	}
	return uuids, nil
}

// Woken reports, per Sleeping VM UUID, whether this host has already woken it.
//
// Woken means the sleeping marker is GONE. Both wake paths — the verb and the wake
// trap — remove it as the FIRST step, before the unit is started, so its absence
// is the authority that a wake has begun; it is the same signal the unit's
// ConditionPathExists=! keys on. A VM whose directory is already gone (a racing
// terminate) reads as woken too, and the controller only acts on rows it still
// believes are Sleeping.
//
// The host is the authority for a wake HAVING happened — only it saw the inbound
// SYN — and this verb is how that fact reaches the control plane within one poll
// cycle. It changes nothing.
//
// A marker that could not be READ fails the whole probe, and that is the one place
// this differs from the Python's os.path.exists, which cannot tell "absent" from
// "could not look". Answering "woken" on a question nobody could put to the host
// makes Atlas flip a Sleeping row to Running for a VM that is still asleep — an
// observation laundered into status, which is the exact failure Boat exists to
// end. The controller runs this as a probe that logs and retries a minute later,
// so a loud failure costs one poll cycle and nothing else.
func Woken(ctx context.Context, runner *run.Runner, uuids []string) (map[string]bool, error) {
	return newParker(runner).woken(ctx, uuids)
}

func (parker *parker) woken(ctx context.Context, uuids []string) (map[string]bool, error) {
	woken := map[string]bool{}
	for _, uuid := range uuids {
		if !paths.IsUUID(uuid) {
			return nil, fmt.Errorf("probe-woken-vms: %q is not a VM UUID", uuid)
		}
		sleeping, err := parker.sleepingMarkerPresent(ctx, uuid)
		if err != nil {
			return nil, err
		}
		woken[uuid] = !sleeping
	}
	return woken, nil
}
