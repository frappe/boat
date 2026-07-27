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
// This is not the operation record. internal/store owns an operation's status,
// because a status kept in two places is two truths that disagree exactly when a
// crash lands between the two writes — which is the case this package exists
// for. The journal owns the decisions taken under an operation, and the
// knowledge of which run of this daemon took them.
//
// # Where the decisions live, and how that differs from the contract
//
// The WO-2 contract has New(store *store.Store) *Journal, and spec/33-boat.md
// §11.5 asks for the decision and the state it justifies to commit in one bbolt
// transaction. That is the right design, and this package cannot reach it from
// outside internal/store: the store's *bbolt.DB is unexported, it has no
// decisions bucket, its operations bucket cannot be listed, and bbolt holds an
// exclusive lock on its file so a second handle on the same path will not open.
// Until internal/store grows a decisions bucket, this package keeps its own
// bbolt file — New takes its path — and every bbolt detail is confined to
// bolt.go so that the move is a file being deleted rather than a rewrite.
//
// Two consequences, both stated again where they bite. Record is still durable
// before it returns, which is the property the write-ahead rule actually needs;
// it is not atomic with a store write, so a crash between them leaves a decision
// recorded for an operation whose outcome was not, and a replay reads the
// decision and finishes — the safe direction. And Unfinished sees only
// operations that recorded a decision.
package journal

import (
	"time"

	"github.com/frappe/boat/internal/store"
	"go.etcd.io/bbolt"
)

// Decision is a choice that cannot be re-made safely: which address was taken,
// which volume was created, which host slot was claimed.
//
// Values is a map of strings rather than a typed payload for the same reason the
// store writes indented JSON: on a host too wedged to answer its own API, the
// only tools an operator has are `strings` and a hex dump, and the answer they
// need out of this file — which address did this VM get — has to be legible in
// both. A verb that grows a second decided value also grows no migration.
type Decision struct {
	OperationID string            `json:"operation_id"`
	Step        string            `json:"step"`
	Values      map[string]string `json:"values"`
	At          time.Time         `json:"at"`
}

// Journal is this host's write-ahead record of decisions.
type Journal struct {
	decisions  *bbolt.DB
	operations *store.Store
	// incarnation is which run of this daemon opened the journal. It is the whole
	// of how an operation a crash abandoned is told apart from one that is merely
	// slow; see Unfinished.
	incarnation int64
}

// New opens the journal's file at path and takes database as the authority on
// what an operation's status is.
//
// Opening is itself a durable act: it advances the incarnation counter, so every
// decision recorded from here on is stamped with a number no earlier run can
// have used. A journal that failed to record that would report its own live
// operations as crashed ones the next time it was asked.
func New(database *store.Store, path string) (*Journal, error) {
	decisions, err := openDecisions(path)
	if err != nil {
		return nil, err
	}
	incarnation, err := beginIncarnation(decisions)
	if err != nil {
		decisions.Close()
		return nil, err
	}
	return &Journal{decisions: decisions, operations: database, incarnation: incarnation}, nil
}

// Close releases the journal's file lock. Reopening the same path afterwards
// returns the same decisions under a new incarnation, which is precisely what a
// restart is.
func (journal *Journal) Close() error { return journal.decisions.Close() }
