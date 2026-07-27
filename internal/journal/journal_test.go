package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frappe/boat/internal/store"
)

// Every test in this package runs against real bbolt files under t.TempDir, the
// journal's and the store's. Durability is the entire job here, so a fake of the
// thing that persists would test nothing that matters — and the tests that close
// the journal and open it again are the only ones that prove a decision is on
// disk rather than in a struct.

const (
	firstOperation  = "task-aaaa1111"
	secondOperation = "task-bbbb2222"
	virtualMachine  = "11111111-2222-3333-4444-555555555555"
)

// host is one host's pair of files: the store that owns operation status and the
// journal path that survives being reopened.
type host struct {
	store       *store.Store
	journalPath string
}

func newHost(t *testing.T) host {
	t.Helper()
	directory := t.TempDir()
	database, err := store.Open(filepath.Join(directory, "boat.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return host{store: database, journalPath: filepath.Join(directory, "journal.db")}
}

// restart opens the journal again, which is what a daemon restart is: the same
// file under the next incarnation.
func (host host) restart(t *testing.T) *Journal {
	t.Helper()
	journal, err := New(host.store, host.journalPath)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	t.Cleanup(func() { journal.Close() })
	return journal
}

// claim leaves an operation in the store the way a verb about to run does.
func (host host) claim(t *testing.T, identifier string) {
	t.Helper()
	if _, claimed, err := host.store.ClaimOperation(identifier, "start-vm", virtualMachine); err != nil || !claimed {
		t.Fatalf("claim %s: claimed=%v err=%v", identifier, claimed, err)
	}
}

func decisionOf(operationID, step string, values map[string]string) Decision {
	return Decision{OperationID: operationID, Step: step, Values: values}
}

func record(t *testing.T, journal *Journal, decision Decision) {
	t.Helper()
	if err := journal.Record(decision); err != nil {
		t.Fatalf("record %s/%s: %v", decision.OperationID, decision.Step, err)
	}
}

func decisionsOf(t *testing.T, journal *Journal, operationID string) []Decision {
	t.Helper()
	decisions, err := journal.Decisions(operationID)
	if err != nil {
		t.Fatalf("read decisions of %s: %v", operationID, err)
	}
	return decisions
}

func steps(decisions []Decision) []string {
	names := []string{}
	for _, decision := range decisions {
		names = append(names, decision.Step)
	}
	return names
}

// The property the whole package is for: what was decided before the crash is
// what is read after it, in the order it was decided.
func TestDecisionsReplayInOrderAfterAReopen(t *testing.T) {
	host := newHost(t)
	journal := host.restart(t)
	record(t, journal, decisionOf(firstOperation, "allocate-address", map[string]string{"ipv6": "2001:db8::5"}))
	record(t, journal, decisionOf(firstOperation, "create-volume", map[string]string{"lv": "atlas-vm-1"}))
	record(t, journal, decisionOf(firstOperation, "claim-slot", map[string]string{"slot": "3"}))
	if err := journal.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}

	replayed := decisionsOf(t, host.restart(t), firstOperation)
	expected := []string{"allocate-address", "create-volume", "claim-slot"}
	if got := steps(replayed); !equal(got, expected) {
		t.Fatalf("replayed steps = %v, want %v", got, expected)
	}
	if address := replayed[0].Values["ipv6"]; address != "2001:db8::5" {
		t.Fatalf("replayed address = %q, want the one that was decided before the crash", address)
	}
}

// A tenth decision must not sort before a ninth, which is what the zero-padded
// sequence in the key buys and what a plain %d would quietly break.
func TestDecisionsStayInOrderPastTenEntries(t *testing.T) {
	host := newHost(t)
	journal := host.restart(t)
	expected := []string{}
	for step := 1; step <= 12; step++ {
		name := "step-" + string(rune('a'+step-1))
		record(t, journal, decisionOf(firstOperation, name, nil))
		expected = append(expected, name)
	}
	if got := steps(decisionsOf(t, journal, firstOperation)); !equal(got, expected) {
		t.Fatalf("steps = %v, want %v", got, expected)
	}
}

// A prefix scan that read a neighbouring operation's entries would hand a
// resumed verb somebody else's address.
func TestDecisionsAreScopedToOneOperation(t *testing.T) {
	host := newHost(t)
	journal := host.restart(t)
	record(t, journal, decisionOf(firstOperation, "first-a", nil))
	record(t, journal, decisionOf(secondOperation, "second-a", nil))
	record(t, journal, decisionOf(firstOperation, "first-b", nil))

	if got := steps(decisionsOf(t, journal, firstOperation)); !equal(got, []string{"first-a", "first-b"}) {
		t.Fatalf("first operation's steps = %v", got)
	}
	if got := steps(decisionsOf(t, journal, secondOperation)); !equal(got, []string{"second-a"}) {
		t.Fatalf("second operation's steps = %v", got)
	}
}

// The answer a first attempt gets, and the answer a replay gets for a step it
// never reached: nothing decided, and no error about it.
func TestDecisionsOfAnOperationThatDecidedNothing(t *testing.T) {
	host := newHost(t)
	decisions, err := host.restart(t).Decisions(firstOperation)
	if err != nil {
		t.Fatalf("read decisions: %v", err)
	}
	if len(decisions) != 0 {
		t.Fatalf("decisions = %v, want none", decisions)
	}
}

func TestRecordRefusesADecisionItCouldNotHandBack(t *testing.T) {
	cases := map[string]Decision{
		"no operation":         decisionOf("", "allocate-address", nil),
		"separator in the key": decisionOf("task/1", "allocate-address", nil),
		"no step":              decisionOf(firstOperation, "", nil),
	}
	journal := newHost(t).restart(t)
	for name, decision := range cases {
		t.Run(name, func(t *testing.T) {
			if err := journal.Record(decision); err == nil {
				t.Fatal("recorded a decision that could never be read back")
			}
		})
	}
}

func TestRecordStampsTheTimeTheCallerLeftZero(t *testing.T) {
	host := newHost(t)
	journal := host.restart(t)
	record(t, journal, decisionOf(firstOperation, "allocate-address", nil))
	stamped := decisionsOf(t, journal, firstOperation)[0].At
	if stamped.IsZero() {
		t.Fatal("the decision came back with no time on it")
	}
	if stamped.Location() != time.UTC {
		t.Fatalf("stamped in %v, want UTC so hosts compare", stamped.Location())
	}
}

// The file is read by `strings` on a host too wedged to answer its API, so the
// identifier has to be in the key and the values have to be legible.
func TestTheFileIsLegibleToAnOperator(t *testing.T) {
	host := newHost(t)
	journal := host.restart(t)
	record(t, journal, decisionOf(firstOperation, "allocate-address", map[string]string{"ipv6": "2001:db8::5"}))
	if err := journal.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	contents := readFile(t, host.journalPath)
	for _, wanted := range []string{firstOperation, "allocate-address", "2001:db8::5"} {
		if !strings.Contains(contents, wanted) {
			t.Fatalf("the journal file does not contain %q", wanted)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(contents)
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
