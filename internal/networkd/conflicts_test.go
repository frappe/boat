package networkd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A frozen clock, so the wall-clock `at` in an event is a value the test can assert.
func frozenClock() func() float64 { return func() float64 { return 1700000000 } }

// A newly-conflicting /128 emits a start; when it clears, an end carries the origins
// that were contending at the moment it started.
func TestObserveConflictsEmitsStartThenEnd(t *testing.T) {
	tracker := NewConflictTracker("")
	tracker.now = frozenClock()

	started := tracker.ObserveConflicts(map[IP6][]HostID{"fdab::x": {"host-a", "host-b"}})
	if len(started) != 1 || started[0].Kind != "start" || started[0].PrivateIP != "fdab::x" {
		t.Fatalf("expected one start event, got %+v", started)
	}
	if !reflect.DeepEqual(started[0].Origins, []HostID{"host-a", "host-b"}) {
		t.Fatalf("start origins = %v, want [host-a host-b]", started[0].Origins)
	}
	if started[0].At != 1700000000 {
		t.Fatalf("start at = %v, want the frozen clock", started[0].At)
	}

	ended := tracker.ObserveConflicts(map[IP6][]HostID{})
	if len(ended) != 1 || ended[0].Kind != "end" || ended[0].PrivateIP != "fdab::x" {
		t.Fatalf("expected one end event, got %+v", ended)
	}
	if !reflect.DeepEqual(ended[0].Origins, []HostID{"host-a", "host-b"}) {
		t.Fatalf("end origins = %v, want the remembered contenders", ended[0].Origins)
	}
}

// Observe populates each event's origins from the advertisements that produced the
// table.
func TestObservePopulatesOriginsFromAdvertisements(t *testing.T) {
	tracker := NewConflictTracker("")
	tracker.now = frozenClock()
	latest := map[HostID]OwnershipAdvertisement{
		"host-a": OwningAdvertisement("host-a", 1, []IP6{"fdab::x"}),
		"host-b": OwningAdvertisement("host-b", 1, []IP6{"fdab::x"}),
	}
	table := EffectiveOwnership(latest)
	events := tracker.Observe(table, latest)
	if len(events) != 1 {
		t.Fatalf("expected one start, got %+v", events)
	}
	if !reflect.DeepEqual(events[0].Origins, []HostID{"host-a", "host-b"}) {
		t.Fatalf("origins = %v, want both advertisers", events[0].Origins)
	}
}

// The on-disk line is the {kind, private_ip, origins, at} contract Atlas may read:
// sorted keys, sorted origins, one line per event.
func TestConflictLogLineShapeIsTheContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conflicts.jsonl")
	tracker := NewConflictTracker(path)
	tracker.now = frozenClock()

	tracker.ObserveConflicts(map[IP6][]HostID{"fdab::x": {"host-b", "host-a"}})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the conflict log: %v", err)
	}
	want := `{"at":1700000000,"kind":"start","origins":["host-a","host-b"],"private_ip":"fdab::x"}` + "\n"
	if string(data) != want {
		t.Fatalf("conflict log line:\n got: %q\nwant: %q", string(data), want)
	}
}

// An in-process subscriber sees every event, which is how the daemon drives its
// metrics counter.
func TestSubscribersReceiveEvents(t *testing.T) {
	tracker := NewConflictTracker("")
	tracker.now = frozenClock()
	var seen []ConflictEvent
	tracker.Subscribe(func(event ConflictEvent) { seen = append(seen, event) })

	tracker.ObserveConflicts(map[IP6][]HostID{"fdab::x": {"host-a"}})
	tracker.ObserveConflicts(map[IP6][]HostID{})

	if len(seen) != 2 || seen[0].Kind != "start" || seen[1].Kind != "end" {
		t.Fatalf("subscriber saw %+v, want a start then an end", seen)
	}
}
