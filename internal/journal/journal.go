// Package journal records a non-idempotent decision BEFORE the side effect it
// authorizes, so that a crash and then a retry replays the decision instead of
// making a second, different one.
//
// A verb that allocates an address, creates a logical volume or claims a host
// slot has made a choice its own retry cannot repeat: run again from the top it
// picks a different address, and the first one is left allocated in nobody's
// book while a half-built netns still carries it. Writing the choice down first
// turns the choice into a lookup — the resumed verb reads what it already
// decided and finishes that same job. That is what makes "every verb is
// idempotent, so retry means re-run" true of the verbs that choose
// (spec/33-boat.md §11.5), and it is the only reason a forward-only operation
// can be resumed at its checkpoint at all (§3.2).
//
// # What is here, and what is in internal/store
//
// The bytes are the store's. Decisions live in the store's own bbolt file, in a
// bucket beside the operations they belong to, because §11.5 asks for the
// decision and the state it justifies to commit in one transaction and bbolt
// takes an exclusive lock per file — a journal with a file of its own could not
// reach that at all, and the second commit was a window a crash could land in.
// So internal/store owns durability, the key layout and the refusal of a
// decision naming work this host is not doing; all three are properties of the
// transaction a decision commits in.
//
// What is here is the rule. Record is the entry point a verb calls, and its
// contract is an ORDER rather than a data structure: record, then do the thing.
// Unfinished is the other half — which operations a crash left behind — and it
// is policy this package would have to state even if the store held every byte,
// because "unfinished" is not "not finished yet".
package journal

import (
	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/store"
)

// Decision is the persisted shape from internal/model, named here because this
// is the package that gives it its meaning. An alias and not a copy: a decision
// written through Record and a decision read back out of the store are one
// value, not two that have to be kept in step.
type Decision = model.Decision

// Journal is this host's write-ahead record of decisions.
type Journal struct {
	store *store.Store
}

// New takes the store as the authority on operations, their status and their
// decisions.
func New(database *store.Store) *Journal { return &Journal{store: database} }

// Record writes the decision durably, and returns only once it survives a
// crash — a decision still sitting in a buffer has not been made, and a caller
// that acted on one is a caller whose retry chooses differently.
//
// The order this is used in is the entire rule: record, then do the thing. A
// decision recorded after its side effect is not a write-ahead journal, it is a
// log — and a log cannot answer the only question a replay has, which is what
// the first attempt chose. Nothing below can enforce that order; it is enforced
// by the caller putting this line above the one that acts.
func (journal *Journal) Record(decision Decision) error {
	return journal.store.RecordDecision(decision)
}

// Decisions returns what an operation already decided, in the order it decided
// it, so a resumed operation re-enters at its checkpoint rather than at its
// beginning.
func (journal *Journal) Decisions(operationID string) ([]Decision, error) {
	return journal.store.Decisions(operationID)
}
