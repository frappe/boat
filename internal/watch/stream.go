package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
)

// ErrNotStreamable means the response writer cannot flush, so events would sit
// in a buffer until enough of them accumulated to spill. It is returned before
// anything is written, so the caller can still answer with a refusal.
var ErrNotStreamable = errors.New("this response writer cannot stream events")

// ServeStream writes events to writer in text/event-stream framing until ctx
// ends — which, for an HTTP handler, is when the client disconnects.
//
// Every frame is flushed. An SSE frame held in net/http's buffer until 4KB have
// piled up is exactly the latency this endpoint exists to remove, and a host
// that emits one event an hour would emit its first one next week.
func (hub *Hub) ServeStream(ctx context.Context, writer http.ResponseWriter) error {
	flusher, streamable := writer.(http.Flusher)
	if !streamable {
		return ErrNotStreamable
	}
	// Subscribe before the headers go out: once the client has a response it is
	// entitled to assume the next change reaches it, and a subscription taken
	// after that would lose everything published in between.
	events, cancel := hub.Subscribe()
	defer cancel()
	writeStreamHeaders(writer)
	flusher.Flush()
	return hub.stream(ctx, writer, flusher, events)
}

func (hub *Hub) stream(ctx context.Context, writer http.ResponseWriter, flusher http.Flusher, events <-chan Event) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case event, open := <-events:
			if !open {
				// Dropped for falling behind. The client reconnects and re-reads
				// the export, which is the whole reason dropping is allowed.
				return nil
			}
			frame, err := frame(event)
			if err != nil {
				return err
			}
			if _, err := writer.Write(frame); err != nil {
				// The client left mid-frame. That ends the stream and nothing
				// else: the export is the backstop for everything it missed.
				slog.Debug("an event stream ended mid-frame", "uuid", event.UUID, "error", err)
				return nil
			}
			flusher.Flush()
		}
	}
}

func writeStreamHeaders(writer http.ResponseWriter) {
	header := writer.Header()
	header.Set("Content-Type", "text/event-stream")
	// A cache between Boat and its watcher would turn a latency channel into a
	// batch one, and a stale event is worse than none: it names an epoch that
	// has already moved.
	header.Set("Cache-Control", "no-cache")
	writer.WriteHeader(http.StatusOK)
}

// frame renders one event: the kind as the SSE event name, the whole event as
// the data line, and the blank line that terminates a frame. The epoch rides
// inside the JSON rather than in the SSE id field because it is not unique —
// several changes can be observed at one epoch.
func frame(event Event) ([]byte, error) {
	document, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("could not encode a %q event for %s: %w", event.Kind, event.UUID, err)
	}
	var rendered bytes.Buffer
	if event.Kind != "" {
		fmt.Fprintf(&rendered, "event: %s\n", event.Kind)
	}
	// json.Marshal never emits a raw newline, so the document is always one
	// data line and needs no continuation handling.
	fmt.Fprintf(&rendered, "data: %s\n\n", document)
	return rendered.Bytes(), nil
}
