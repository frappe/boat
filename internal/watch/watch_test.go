package watch

import (
	"sync"
	"testing"
	"time"
)

// awaitDelivery bounds every wait in this package's tests: a hub that has to be
// waited on for a second has already failed the only promise it makes.
const awaitDelivery = 2 * time.Second

func TestPublishReachesEverySubscriber(t *testing.T) {
	hub := NewHub()
	first, cancelFirst := hub.Subscribe()
	defer cancelFirst()
	second, cancelSecond := hub.Subscribe()
	defer cancelSecond()

	hub.Publish(Event{Kind: KindVirtualMachine, UUID: "vm-1", ObservedEpoch: 4})

	for _, events := range []<-chan Event{first, second} {
		select {
		case event := <-events:
			if event.UUID != "vm-1" || event.ObservedEpoch != 4 {
				t.Errorf("got %+v, want the published event", event)
			}
		case <-time.After(awaitDelivery):
			t.Fatal("a subscriber never received the published event")
		}
	}
}

// The promise the whole package rests on: a watcher may lose freshness, and may
// never stall the goroutine that has just changed a VM.
func TestASlowSubscriberIsDroppedAndNeverBlocksPublish(t *testing.T) {
	hub := NewHub()
	events, cancel := hub.Subscribe()
	defer cancel()

	published := make(chan struct{})
	go func() {
		defer close(published)
		for range subscriberBacklog * 3 {
			hub.Publish(Event{Kind: KindVirtualMachine, UUID: "slow"})
		}
	}()

	select {
	case <-published:
	case <-time.After(awaitDelivery):
		t.Fatal("a subscriber that read nothing blocked the publisher")
	}
	received := 0
	// The range ends because the drop closed the channel, which is how the
	// subscriber's client learns to reconnect and re-read the export.
	for range events {
		received++
	}
	if received != subscriberBacklog {
		t.Errorf("got %d events before the drop, want the backlog of %d", received, subscriberBacklog)
	}
}

func TestCancelReleasesTheSubscriptionAndToleratesARepeat(t *testing.T) {
	hub := NewHub()
	events, cancel := hub.Subscribe()

	cancel()
	// A stream cancels on its way out whether or not Publish already dropped it,
	// so the second call must be a no-op rather than a close of a closed channel.
	cancel()
	hub.Publish(Event{Kind: KindVirtualMachine, UUID: "vm-1"})

	if _, open := <-events; open {
		t.Error("a released subscriber still received an event")
	}
	if subscribers := hub.subscriberCount(); subscribers != 0 {
		t.Errorf("got %d subscribers after cancel, want 0", subscribers)
	}
}

func TestPublishSubscribeAndCancelRunConcurrently(t *testing.T) {
	hub := NewHub()
	var running sync.WaitGroup
	for range 8 {
		running.Add(2)
		go func() {
			defer running.Done()
			for range 50 {
				hub.Publish(Event{Kind: KindVirtualMachine, UUID: "vm-1"})
			}
		}()
		go func() {
			defer running.Done()
			events, cancel := hub.Subscribe()
			// The reader races the publishers; the cancel races the reader. The
			// range ends when the cancel closes the channel, whether or not
			// Publish had already dropped it.
			read := make(chan struct{})
			go func() {
				defer close(read)
				for range events {
				}
			}()
			cancel()
			<-read
		}()
	}

	running.Wait()

	if subscribers := hub.subscriberCount(); subscribers != 0 {
		t.Errorf("got %d subscribers once everything finished, want 0", subscribers)
	}
}

func (hub *Hub) subscriberCount() int {
	hub.mutex.Lock()
	defer hub.mutex.Unlock()
	return len(hub.subscribers)
}
