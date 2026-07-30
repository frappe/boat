package api

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/frappe/boat/internal/store"
	"github.com/frappe/boat/internal/wire"
)

// refuseMovedObservation is the compare-and-set gate of §11.2.
//
// A caller that merely re-asserts intent sends no If-Match and is not gated at
// all, which is the common case and has to stay ungated: Atlas PUTs desired
// state before every verb and on every reconnect, and a precondition there would
// make resynchronisation fail precisely when the mirror is furthest behind.
//
// A caller that DECIDED something from the mirror sends the observed epoch it
// decided from, and is refused if this host's record for the VM has been written
// since. That refusal is the whole content of "the mirror is disposable because
// no contended decision is ever taken from it": without it, the sentence is true
// only because nothing has yet been taught to take one.
func (server *Server) refuseMovedObservation(uuid string, header *string) *errorResponse {
	offered, present, err := observedEpochOffered(header)
	if err != nil {
		return badRequest(err.Error())
	}
	if !present {
		return nil
	}
	err = server.state.CheckVirtualMachineUnmoved(uuid, offered)
	switch {
	case errors.Is(err, store.ErrObservationMoved):
		// Logged as well as refused, because the store distinguishes two cases the
		// caller does not need to and an operator does: an epoch a few writes
		// behind, and an epoch this host never issued at all — which means the
		// mirror was built from a DIFFERENT Boat's store. Both are answered "re-read
		// the export", so the sentence is one sentence; only the journal says which.
		slog.Warn("refused a write against a moved observation", "uuid", uuid, "offered", offered, "error", err)
		return conflictBecause(wire.ErrorReasonStaleObservation,
			"This host's observation of "+uuid+" has moved since the epoch offered, so re-read the export and decide again.")
	case err != nil:
		return internalFault("The observed epoch could not be compared.", err)
	}
	return nil
}

// observedEpochOffered reads an If-Match header into the epoch it names.
//
// The value is an observed epoch, optionally wrapped in the quotes an HTTP
// entity tag carries and optionally marked weak, so a caller using an ordinary
// HTTP client's ETag plumbing and one that pastes the number out of an export
// state the same precondition. Nothing else is accepted — including the
// wildcard `*`, which in HTTP means "any current representation" and is
// therefore not a precondition at all, only a way to look like one.
//
// An absent header is not an error and not a zero: it means the caller offered
// no precondition, which is a different request from one that offered epoch 0.
func observedEpochOffered(header *string) (int64, bool, error) {
	if header == nil {
		return 0, false, nil
	}
	value := strings.TrimSpace(*header)
	if value == "" {
		return 0, false, nil
	}
	value = strings.Trim(strings.TrimPrefix(value, "W/"), `"`)
	epoch, err := strconv.ParseInt(value, 10, 64)
	if err != nil || epoch < 0 {
		return 0, false, fmt.Errorf(
			"If-Match must be an observed epoch read from an export, and this request carried %q", *header)
	}
	return epoch, true, nil
}
