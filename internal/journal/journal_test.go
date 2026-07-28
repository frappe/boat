package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/store"
)

// Every test in this package runs against a real bbolt file under t.TempDir.
// Durability is the entire job here, so a fake of the thing that persists would
// test nothing that matters — and the tests that close the store and open it
// again are the only ones that prove a decision is on disk rather than in a
// struct.

const (
	firstOperation  = "task-aaaa1111"
	secondOperation = "task-bbbb2222"
	virtualMachine  = "11111111-2222-3333-4444-555555555555"
)

// host is one host's file, and the sequence of daemon runs over it.
type host struct {
	path  string
	store *store.Store
}

func newHost(t *testing.T) *host {
	t.Helper()
	return &host{path: filepath.Join(t.TempDir(), "boat.db")}
}

// restart closes the store and opens it again, which is what a daemon restart
// is: the same file under the next incarnation. Every operation claimed before
// it belongs to a run that has ended.
func (host *host) restart(t *testing.T) *Journal {
	t.Helper()
	if host.store != nil {
		if err := host.store.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	}
	database, err := store.Open(host.path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	host.store = database
	t.Cleanup(func() { database.Close() })
	return New(database)
}

// claim leaves an operation in the store the way a verb about to run does.
func (host *host) claim(t *testing.T, identifier string) {
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
func TestDecisionsReplayInOrderAfterARestart(t *testing.T) {
	host := newHost(t)
	journal := host.restart(t)
	host.claim(t, firstOperation)
	record(t, journal, decisionOf(firstOperation, "allocate-address", map[string]string{"ipv6": "2001:db8::5"}))
	record(t, journal, decisionOf(firstOperation, "create-volume", map[string]string{"lv": "atlas-vm-1"}))
	record(t, journal, decisionOf(firstOperation, "claim-slot", map[string]string{"slot": "3"}))

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
	host.claim(t, firstOperation)
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
	host.claim(t, firstOperation)
	host.claim(t, secondOperation)
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

// A decision names work this host is doing, or it names nothing. The store
// checks that in the transaction that would have written it, so the two cases
// below cannot leave a decision behind that no replay could ever reach.
func TestRecordRefusesADecisionNoOperationCouldOwn(t *testing.T) {
	host := newHost(t)
	journal := host.restart(t)
	host.claim(t, secondOperation)
	complete(t, host, secondOperation, model.OperationSuccess)
	cases := map[string]Decision{
		"never claimed":        decisionOf(firstOperation, "allocate-address", nil),
		"already finished":     decisionOf(secondOperation, "allocate-address", nil),
		"no operation":         decisionOf("", "allocate-address", nil),
		"separator in the key": decisionOf("task/1", "allocate-address", nil),
		"no step":              decisionOf(secondOperation, "", nil),
	}
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
	host.claim(t, firstOperation)
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
// identifier has to be in the key and the values have to be legible. It is the
// STORE's file: the journal has none of its own, which is what lets a decision
// and the operation that took it commit together.
func TestTheDecisionsAreInTheStoreFileAndLegibleToAnOperator(t *testing.T) {
	host := newHost(t)
	journal := host.restart(t)
	host.claim(t, firstOperation)
	record(t, journal, decisionOf(firstOperation, "allocate-address", map[string]string{"ipv6": "2001:db8::5"}))
	if err := host.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	host.store = nil
	contents := readFile(t, host.path)
	for _, wanted := range []string{firstOperation, "allocate-address", "2001:db8::5"} {
		if !strings.Contains(contents, wanted) {
			t.Fatalf("the store file does not contain %q", wanted)
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
