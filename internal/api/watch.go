package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/watch"
	"github.com/frappe/boat/internal/wire"
)

// Watch hands the connection to the hub for as long as the client holds it open.
func (server *Server) Watch(ctx context.Context, request wire.WatchRequestObject) (wire.WatchResponseObject, error) {
	return &eventStream{ctx: ctx, hub: server.watch}, nil
}

// eventStream is the generated text/event-stream response, written by hand.
//
// The generated one copies from an io.Reader and never flushes, which would hold
// every frame in net/http's buffer until 4KB of them piled up — a latency channel
// delivering yesterday's news. This one hands the ResponseWriter to the hub,
// which frames and flushes each event as it happens.
//
// It carries the request's context, which is a thing a struct is normally not
// allowed to do. Here it is the point: this value exists only to be visited by
// the generated handler on the same request, and the context ending is exactly
// how the stream learns its client hung up.
type eventStream struct {
	ctx context.Context
	hub *watch.Hub
}

func (response *eventStream) VisitWatchResponse(writer http.ResponseWriter) error {
	return response.hub.ServeStream(response.ctx, writer)
}

// PublishObserved tells the watchers what the host was just seen to be.
//
// Both paths that write an observation call it: the post-verb one here, and the
// reconciler, which the daemon wires to this method after the server is built. So
// a change Atlas caused through a verb and one the reconciler noticed on its
// own — a guest that died, a unit that failed, a VM the wake trap resumed — reach
// the stream the same way, which is what lets a watcher trust /watch carries every
// observed change and not only the ones it asked for.
//
// The epoch is read after the write that bumped it, so an event names the state a
// later export would confirm. A hub that cannot be told is not a reason to fail
// the caller that succeeded: watch carries freshness, the export carries truth, and
// the backstop for a missed event is the client re-reading the export.
func (server *Server) PublishObserved(record model.VirtualMachine) {
	epoch, err := server.state.ObservedEpoch()
	if err != nil {
		slog.Error("could not read the observed epoch for a watch event", "uuid", record.UUID, "error", err)
		return
	}
	server.watch.Publish(watch.Event{
		Kind:          watch.KindVirtualMachine,
		UUID:          record.UUID,
		ObservedEpoch: epoch,
		At:            time.Now().UTC(),
		Payload:       virtualMachineToWire(record),
	})
}
