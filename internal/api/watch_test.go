package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/wire"
)

// awaitEvent bounds every wait in these tests. A watch endpoint that has to be
// waited on for a second is not delivering the latency it exists for.
const awaitEvent = 2 * time.Second

// streamedEvent is what a client of /watch decodes. It is written out here
// rather than reused from internal/watch because a wire format nobody restates
// is a wire format nobody notices breaking.
type streamedEvent struct {
	Kind          string              `json:"kind"`
	UUID          string              `json:"uuid"`
	ObservedEpoch int64               `json:"observed_epoch"`
	Payload       wire.VirtualMachine `json:"payload"`
}

func TestWatchStreamsAnObservedChangeToAConnectedClient(t *testing.T) {
	state := newFakeStore()
	state.fence(testUuid, 1)
	// A fence alone no longer permits a boot: a host holding no desired state
	// holds no authority to act on the VM (internal/api/fence.go). This test is
	// about the stream rather than the gate, so it asserts the intent the way
	// Atlas does before every verb.
	state.desire(model.DesiredVirtualMachine{
		UUID: testUuid, BootEpoch: 1, DesiredPower: model.PowerRunning,
	})
	machines := &fakeVirtualMachines{observed: model.VirtualMachine{ObservedStatus: model.StatusRunning}}
	daemon := httptest.NewServer(newTestServer(state, machines).SocketHandler())
	defer daemon.Close()

	stream, err := http.Get(daemon.URL + "/watch")
	if err != nil {
		t.Fatalf("could not open the stream: %v", err)
	}
	defer stream.Body.Close()
	if stream.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("got content type %q, want text/event-stream", stream.Header.Get("Content-Type"))
	}

	// The headers arrived, so the subscription exists: a change made now must
	// reach it.
	start(t, daemon, "Task-30")
	frame := readFrame(t, bufio.NewReader(stream.Body))

	if !strings.Contains(frame, "event: virtual-machine\n") {
		t.Errorf("got %q, want the kind as the event name", frame)
	}
	event := decodeFrame(t, frame)
	if event.UUID != testUuid || event.Kind != "virtual-machine" {
		t.Errorf("got %+v, want the observed change to %s", event, testUuid)
	}
	if event.Payload.ObservedStatus != wire.VirtualMachineStatusRunning {
		t.Errorf("got payload %+v, want the observed record", event.Payload)
	}
	// The epoch is read after the write that bumped it, so a client can tell
	// whether the export it holds already includes this change.
	if event.ObservedEpoch != state.epoch {
		t.Errorf("got epoch %d, want the store's %d", event.ObservedEpoch, state.epoch)
	}
}

func TestWatchEndsWhenItsClientDisconnects(t *testing.T) {
	server := newTestServer(newFakeStore(), &fakeVirtualMachines{})
	ctx, disconnect := context.WithCancel(context.Background())
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/watch", nil).WithContext(ctx)
	served := make(chan struct{})
	go func() {
		defer close(served)
		server.SocketHandler().ServeHTTP(recorder, request)
	}()

	disconnect()

	select {
	case <-served:
	case <-time.After(awaitEvent):
		t.Fatal("the stream outlived the client that asked for it")
	}
	if recorder.Code != http.StatusOK {
		t.Errorf("got %d, want 200", recorder.Code)
	}
}

// A hub is not required to have a Server: watch carries freshness and the export
// carries truth, so a daemon wired without one answers an empty stream rather
// than dereferencing nil in the middle of a verb.
func TestAServerBuiltWithoutAHubStillStreams(t *testing.T) {
	state := newFakeStore()
	server := NewServer(Dependencies{Operations: state, State: state, VirtualMachines: &fakeVirtualMachines{}})
	ctx, disconnect := context.WithCancel(context.Background())
	disconnect()

	recorder := httptest.NewRecorder()
	server.SocketHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/watch", nil).WithContext(ctx))

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
}

func start(t *testing.T, daemon *httptest.Server, identifier string) {
	t.Helper()
	body := strings.NewReader(`{"operation_id":"` + identifier + `"}`)
	response, err := http.Post(daemon.URL+"/vms/"+testUuid+"/start", "application/json", body)
	if err != nil {
		t.Fatalf("could not start the virtual machine: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the start answered %d, want 200", response.StatusCode)
	}
}

// readFrame reads one server-sent event: the lines up to the blank line that
// terminates a frame.
func readFrame(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	framed := make(chan string, 1)
	go func() {
		var frame strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\n" {
				framed <- frame.String()
				return
			}
			frame.WriteString(line)
		}
	}()
	select {
	case frame := <-framed:
		return frame
	case <-time.After(awaitEvent):
		t.Fatal("no event reached the subscriber")
		return ""
	}
}

func decodeFrame(t *testing.T, frame string) streamedEvent {
	t.Helper()
	for line := range strings.SplitSeq(strings.TrimSuffix(frame, "\n"), "\n") {
		document, isData := strings.CutPrefix(line, "data: ")
		if !isData {
			continue
		}
		var event streamedEvent
		if err := json.Unmarshal([]byte(document), &event); err != nil {
			t.Fatalf("could not decode %q: %v", document, err)
		}
		return event
	}
	t.Fatalf("the frame %q carried no data line", frame)
	return streamedEvent{}
}
