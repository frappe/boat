// Conflict detection + operator event surface, ported from conflicts.py (spec §7.3
// / §18.2).
//
// The detector itself lives in EffectiveOwnership (records.go) — a /128 in two
// origins' active sets goes to OwnershipTable.Conflicts and is dropped from routing.
// This adds the operator-visible layer: every conflict START and END emits an event,
// appended as one JSON line to /var/lib/atlas-networkd/conflicts.jsonl so an operator
// can `tail -F` it or ship it through their log pipeline. Subscribers get the same
// events in-process (the daemon wires a metrics counter; tests observe directly).
//
// The on-disk line shape {kind, private_ip, origins, at} IS a contract — Atlas and
// dashboards may read it — so it is written with sorted keys and a sorted origins
// list, byte-for-byte as conflicts.py wrote it.
package networkd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// DefaultConflictsLog is the file-based event sink bootstrap's StateDirectory holds.
const DefaultConflictsLog = "/var/lib/atlas-networkd/conflicts.jsonl"

// ConflictEvent is one transition: a /128 entered (Kind "start") or left (Kind
// "end") the conflicting state. Origins is the set of origin HostIDs whose latest
// advertisements all include this /128 at the moment of the transition; At is the
// wall-clock timestamp in seconds (a controlled clock in tests).
type ConflictEvent struct {
	Kind      string
	PrivateIP IP6
	Origins   []HostID
	At        float64
}

// ConflictTracker remembers the previous conflict set so it can emit END events when
// a conflict clears. Driven once per scan/apply tick — a /128's conflict status only
// changes when the effective table changes. Cheap: O(number of conflicts).
type ConflictTracker struct {
	previous        map[IP6]struct{}
	previousOrigins map[IP6][]HostID
	subscribers     []func(ConflictEvent)
	// now is the injection point tests replace with a controlled clock; production
	// leaves it as wall-clock seconds, which the operator log records.
	now     func() float64
	logPath string
}

// NewConflictTracker builds a tracker writing to logPath (empty disables the file
// sink — a test that only inspects returned events passes "").
func NewConflictTracker(logPath string) *ConflictTracker {
	return &ConflictTracker{
		previous:        map[IP6]struct{}{},
		previousOrigins: map[IP6][]HostID{},
		now:             func() float64 { return float64(time.Now().UnixNano()) / 1e9 },
		logPath:         logPath,
	}
}

// Subscribe registers an in-process callback — the daemon wires a metrics counter
// (increment on start, decrement on end); tests capture events.
func (tracker *ConflictTracker) Subscribe(callback func(ConflictEvent)) {
	tracker.subscribers = append(tracker.subscribers, callback)
}

// Observe diffs the new effective table's conflicts against the previous ones,
// using latestPerOrigin to populate each event's Origins. It is the ownership-only
// entry point; the daemon that also wants render-level mesh_address collisions
// unions them and calls ObserveConflicts directly.
func (tracker *ConflictTracker) Observe(table OwnershipTable, latestPerOrigin map[HostID]OwnershipAdvertisement) []ConflictEvent {
	current := map[IP6][]HostID{}
	for ip := range table.Conflicts {
		var origins []HostID
		for origin, advertisement := range latestPerOrigin {
			if advertisement.Owns(ip) {
				origins = append(origins, origin)
			}
		}
		current[ip] = sortedUnique(origins)
	}
	return tracker.ObserveConflicts(current)
}

// ObserveConflicts is the general entry point: diff a pre-computed CURRENT conflict
// map {private_ip: origins} against the previous one, emitting START events for
// newly-conflicting /128s and END events for cleared ones. The daemon apply path
// hands the UNION of owned-/128 double-ownership (§7.3) and the render-level
// mesh_address collisions (H2) here in one call. Events go to subscribers and the
// jsonl sink; the emitted slice is returned.
func (tracker *ConflictTracker) ObserveConflicts(current map[IP6][]HostID) []ConflictEvent {
	at := tracker.now()
	var events []ConflictEvent
	for _, ip := range sortedKeys(current) {
		if _, wasConflicting := tracker.previous[ip]; !wasConflicting {
			events = append(events, ConflictEvent{Kind: "start", PrivateIP: ip, Origins: current[ip], At: at})
		}
	}
	for _, ip := range sortedKeys(tracker.previous) {
		if _, stillConflicting := current[ip]; !stillConflicting {
			events = append(events, ConflictEvent{Kind: "end", PrivateIP: ip, Origins: tracker.previousOrigins[ip], At: at})
		}
	}
	tracker.previous = keySet(current)
	tracker.previousOrigins = cloneOrigins(current)
	for _, event := range events {
		for _, callback := range tracker.subscribers {
			callback(event)
		}
		tracker.appendLog(event)
	}
	return events
}

// appendLog writes one JSON line per event to the file sink. Best-effort: a
// log-write failure is operationally interesting but never a reason to crash the
// mesh, so it is swallowed (the backstop is the in-process subscriber the daemon
// wires for metrics). The object is marshalled from a map so encoding/json sorts the
// keys, reproducing conflicts.py's sort_keys=True line shape byte-for-byte.
func (tracker *ConflictTracker) appendLog(event ConflictEvent) {
	if tracker.logPath == "" {
		return
	}
	// The on-disk shape sorts origins (conflicts.py wrote sorted(ev.origins)), so
	// the line is stable regardless of the order the caller handed them in — a
	// dashboard diffing the file must not see spurious churn from ordering.
	origins := append([]HostID{}, event.Origins...)
	sort.Strings(origins)
	line, err := json.Marshal(map[string]any{
		"kind":       event.Kind,
		"private_ip": event.PrivateIP,
		"origins":    origins,
		"at":         event.At,
	})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(tracker.logPath), 0o755); err != nil {
		return
	}
	file, err := os.OpenFile(tracker.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	if _, err := file.Write(append(line, '\n')); err != nil {
		return
	}
	_ = file.Sync()
}

// sortedKeys returns a map's keys sorted, so START/END events emit in a stable
// order regardless of Go's randomized map iteration.
func sortedKeys[V any](m map[IP6]V) []IP6 {
	keys := make([]IP6, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// keySet is the set of a map's keys — the previous-conflicts membership set.
func keySet[V any](m map[IP6]V) map[IP6]struct{} {
	set := make(map[IP6]struct{}, len(m))
	for key := range m {
		set[key] = struct{}{}
	}
	return set
}

// cloneOrigins copies the current origins map so a later mutation of a returned
// event's slice cannot reach back into the tracker's remembered state.
func cloneOrigins(current map[IP6][]HostID) map[IP6][]HostID {
	clone := make(map[IP6][]HostID, len(current))
	for ip, origins := range current {
		clone[ip] = append([]HostID(nil), origins...)
	}
	return clone
}
