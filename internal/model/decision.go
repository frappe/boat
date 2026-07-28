package model

import "time"

// Decision is a choice an operation made that its own retry could not repeat:
// which address was taken, which volume was created, which filesystem a rebuild
// was authorized to lay down.
//
// This is only the persisted shape. internal/journal is where the rule lives —
// a decision is recorded BEFORE the side effect it authorizes, so that a crash
// and then a retry replays the choice instead of making a second, different one
// (spec/33-boat.md §11.5).
//
// Values is a map of strings rather than a typed payload for the same reason
// every record here is stored as indented JSON: on a host too wedged to answer
// its own API the only tools an operator has are `strings` and a hex dump, and
// the answer they need out of the file — which address did this VM get — has to
// be legible in both. A verb that grows a second decided value grows no
// migration with it.
type Decision struct {
	OperationID string            `json:"operation_id"`
	Step        string            `json:"step"`
	Values      map[string]string `json:"values"`
	At          time.Time         `json:"at"`
}
