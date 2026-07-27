// Package watch fans observed changes out to subscribers as server-sent events.
//
// It carries latency, never truth. A dropped stream costs a watcher freshness
// and the whole-host export restores it — which is what lets the hub drop a
// subscriber that cannot keep up rather than wait for it. The goroutine
// publishing an event has just changed a VM, so a watcher that could stall a
// publisher is a watcher that could stop the host from running VMs.
package watch

import (
	"sync"
	"time"
)

// The kinds an event carries. A subscriber that only cares about one of them
// filters on the SSE event name rather than decoding every payload.
const (
	KindVirtualMachine = "virtual-machine"
	KindOperation      = "operation"
)

// subscriberBacklog is how far behind a subscriber may fall before it is
// dropped. It is deliberately small: a watcher exists for latency, and one that
// is dozens of events behind is no longer delivering any — it is holding memory
// the daemon needs to run VMs. Being dropped costs its client one export.
const subscriberBacklog = 32

// Event is one observed change.
//
// ObservedEpoch is what makes a reconnect cheap: a client compares the last
// epoch it saw with the one the export carries and knows whether it missed
// anything, so it never has to guess.
type Event struct {
	Kind          string    `json:"kind"`
	UUID          string    `json:"uuid"`
	ObservedEpoch int64     `json:"observed_epoch"`
	At            time.Time `json:"at"`
	Payload       any       `json:"payload"`
}

// Hub is the fan-out. It owns the subscriber set and nothing else: it does not
// remember events, because a watcher that missed one re-reads the export.
type Hub struct {
	mutex       sync.Mutex
	subscribers map[chan Event]struct{}
}

func NewHub() *Hub {
	return &Hub{subscribers: map[chan Event]struct{}{}}
}

// Publish never blocks on a slow subscriber. A send that would wait drops the
// subscriber instead; its client sees the stream end, reconnects, and re-reads
// the export. Freshness is what a watcher loses, never truth.
func (hub *Hub) Publish(event Event) {
	hub.mutex.Lock()
	defer hub.mutex.Unlock()
	for events := range hub.subscribers {
		select {
		case events <- event:
		default:
			hub.drop(events)
		}
	}
}

// Subscribe registers a subscriber and returns it with the cancel that releases
// it. The channel is closed when the subscription ends, whether the subscriber
// let go or the hub dropped it, so a reader learns of both the same way.
func (hub *Hub) Subscribe() (events <-chan Event, cancel func()) {
	subscriber := make(chan Event, subscriberBacklog)
	hub.mutex.Lock()
	hub.subscribers[subscriber] = struct{}{}
	hub.mutex.Unlock()
	return subscriber, func() { hub.release(subscriber) }
}

// release is idempotent, because a stream cancels on its way out whether it
// ended by its own choice or because Publish had already dropped it. Closing a
// channel twice panics, and a panic in a daemon that supervises live VMs takes
// the VMs with it.
func (hub *Hub) release(subscriber chan Event) {
	hub.mutex.Lock()
	defer hub.mutex.Unlock()
	if _, subscribed := hub.subscribers[subscriber]; subscribed {
		hub.drop(subscriber)
	}
}

// drop forgets a subscriber and closes its channel. The caller holds the mutex,
// which is what makes the close safe: no send can be in flight on a channel
// nobody can reach the map to find.
func (hub *Hub) drop(subscriber chan Event) {
	delete(hub.subscribers, subscriber)
	close(subscriber)
}
