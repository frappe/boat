package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubWriter is an http.ResponseWriter that records what a stream wrote and how
// often it flushed, and can be read while the stream is still running.
type stubWriter struct {
	mutex   sync.Mutex
	headers http.Header
	status  int
	body    bytes.Buffer
	flushed chan struct{}
	// hold, when set, stops every Write until it is closed: a client that has
	// stopped reading, as far as the stream can tell.
	hold    chan struct{}
	flushes int
}

func newStubWriter() *stubWriter {
	return &stubWriter{headers: http.Header{}, flushed: make(chan struct{}, 64)}
}

func (writer *stubWriter) Header() http.Header { return writer.headers }

func (writer *stubWriter) WriteHeader(status int) {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	writer.status = status
}

func (writer *stubWriter) Write(frame []byte) (int, error) {
	if writer.hold != nil {
		<-writer.hold
	}
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.body.Write(frame)
}

func (writer *stubWriter) Flush() {
	writer.mutex.Lock()
	writer.flushes++
	writer.mutex.Unlock()
	select {
	case writer.flushed <- struct{}{}:
	default:
	}
}

func (writer *stubWriter) awaitFlush(t *testing.T) {
	t.Helper()
	select {
	case <-writer.flushed:
	case <-time.After(awaitDelivery):
		t.Fatal("the stream never flushed, so nothing it wrote reached the client")
	}
}

func (writer *stubWriter) text() string {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.body.String()
}

func (writer *stubWriter) wrote() (status int, flushes int) {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.status, writer.flushes
}

func TestServeStreamFramesAndFlushesEveryEvent(t *testing.T) {
	hub := NewHub()
	writer := newStubWriter()
	ctx, disconnect := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- hub.ServeStream(ctx, writer) }()
	// The headers are flushed before the loop starts, so their arrival is how a
	// test knows the subscription this hub is about to publish to exists.
	writer.awaitFlush(t)

	hub.Publish(Event{Kind: KindVirtualMachine, UUID: "vm-1", ObservedEpoch: 9, At: time.Now().UTC()})
	writer.awaitFlush(t)

	frame := writer.text()
	if !strings.HasPrefix(frame, "event: virtual-machine\n") {
		t.Errorf("got %q, want the kind as the SSE event name", frame)
	}
	if !strings.HasSuffix(frame, "\n\n") {
		t.Errorf("got %q, want a frame terminated by a blank line", frame)
	}
	var event Event
	document := strings.TrimSpace(strings.SplitN(frame, "data: ", 2)[1])
	if err := json.Unmarshal([]byte(document), &event); err != nil {
		t.Fatalf("could not decode %q: %v", document, err)
	}
	if event.UUID != "vm-1" || event.ObservedEpoch != 9 {
		t.Errorf("got %+v, want the published event", event)
	}
	if writer.headers.Get("Content-Type") != "text/event-stream" {
		t.Errorf("got content type %q, want text/event-stream", writer.headers.Get("Content-Type"))
	}
	// Once for the headers and once for the event: an event held back for the
	// next 4KB of traffic is the latency this endpoint exists to remove.
	if status, flushes := writer.wrote(); status != http.StatusOK || flushes != 2 {
		t.Errorf("got status %d and %d flushes, want 200 and one flush per frame", status, flushes)
	}

	disconnect()
	if err := <-served; err != nil {
		t.Errorf("a stream its client hung up on reported %v, want nothing", err)
	}
}

func TestServeStreamEndsWhenItsClientDisconnects(t *testing.T) {
	hub := NewHub()
	writer := newStubWriter()
	ctx, disconnect := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- hub.ServeStream(ctx, writer) }()
	writer.awaitFlush(t)

	disconnect()

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("got %v, want a clean end", err)
		}
	case <-time.After(awaitDelivery):
		t.Fatal("the stream outlived the client that asked for it")
	}
	if subscribers := hub.subscriberCount(); subscribers != 0 {
		t.Errorf("got %d subscribers after the stream ended, want 0", subscribers)
	}
}

// The promise from the client's side: a stream whose reader has stopped reading
// is dropped rather than allowed to hold up whoever is publishing.
func TestAStreamThatStoppedReadingIsDroppedAndEnds(t *testing.T) {
	hub := NewHub()
	writer := newStubWriter()
	writer.hold = make(chan struct{})
	ctx, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	served := make(chan error, 1)
	go func() { served <- hub.ServeStream(ctx, writer) }()
	writer.awaitFlush(t)

	published := make(chan struct{})
	go func() {
		defer close(published)
		for range subscriberBacklog * 2 {
			hub.Publish(Event{Kind: KindVirtualMachine, UUID: "vm-1"})
		}
	}()
	select {
	case <-published:
	case <-time.After(awaitDelivery):
		t.Fatal("a stream stuck writing to its client blocked the publisher")
	}
	close(writer.hold)

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("got %v, want a clean end", err)
		}
	case <-time.After(awaitDelivery):
		t.Fatal("a dropped stream never ended")
	}
}

func TestServeStreamRefusesAWriterItCannotFlush(t *testing.T) {
	hub := NewHub()
	writer := &nonFlusher{}

	err := hub.ServeStream(context.Background(), writer)

	if !errors.Is(err, ErrNotStreamable) {
		t.Fatalf("got %v, want ErrNotStreamable", err)
	}
	if writer.wrote {
		t.Error("the refusal wrote to the response, so the caller can no longer answer with one")
	}
	if subscribers := hub.subscriberCount(); subscribers != 0 {
		t.Errorf("got %d subscribers after a refusal, want 0", subscribers)
	}
}

// nonFlusher is an http.ResponseWriter and nothing more.
type nonFlusher struct {
	headers http.Header
	wrote   bool
}

func (writer *nonFlusher) Header() http.Header {
	if writer.headers == nil {
		writer.headers = http.Header{}
	}
	return writer.headers
}

func (writer *nonFlusher) Write(frame []byte) (int, error) {
	writer.wrote = true
	return len(frame), nil
}

func (writer *nonFlusher) WriteHeader(status int) { writer.wrote = true }
