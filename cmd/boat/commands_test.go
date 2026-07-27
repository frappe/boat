package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frappe/boat/internal/wire"
)

// startFakeDaemon serves a stand-in for the daemon on a socket of its own and
// points the CLI at it. The CLI reaches it exactly as it reaches a real Boat —
// there is only the one path — so these tests exercise the real client.
func startFakeDaemon(t *testing.T, handler http.Handler) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "boat.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("could not listen on %s: %v", path, err)
	}
	server := &http.Server{Handler: handler}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	t.Setenv("BOAT_SOCKET", path)
}

func writeJSON(t *testing.T, writer http.ResponseWriter, body any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(body); err != nil {
		t.Errorf("could not write the response: %v", err)
	}
}

func TestVmLsPrintsWhatTheHostObserved(t *testing.T) {
	active := "active"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms", func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, []wire.VirtualMachine{{
			Uuid:            "vm-one",
			ObservedStatus:  wire.VirtualMachineStatusRunning,
			ObservedAt:      time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC),
			UnitActiveState: &active,
		}})
	})
	startFakeDaemon(t, mux)
	var output, errorOutput bytes.Buffer

	if code := run([]string{"vm", "ls"}, &output, &errorOutput); code != exitSuccess {
		t.Fatalf("got exit %d, want 0: %s", code, errorOutput.String())
	}
	for _, want := range []string{"UUID", "vm-one", "Running", "active"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("the listing does not mention %q: %s", want, output.String())
		}
	}
}

func TestVmStartPrintsTheHostsTrace(t *testing.T) {
	trace := "+ systemctl start firecracker-vm@vm-one.service\n"
	exitCode := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/vms/vm-one/start", func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, wire.Operation{
			OperationId: "cli-1", Verb: "start-vm", Uuid: "vm-one",
			Status: wire.OperationStatusSuccess, Output: &trace, ExitCode: &exitCode,
		})
	})
	startFakeDaemon(t, mux)
	var output, errorOutput bytes.Buffer

	if code := run([]string{"vm", "start", "vm-one"}, &output, &errorOutput); code != exitSuccess {
		t.Fatalf("got exit %d, want 0: %s", code, errorOutput.String())
	}
	if !strings.Contains(output.String(), trace) {
		t.Errorf("the operator was not shown the trace: %q", output.String())
	}
}

// A failed operation is a successful request, so the CLI has to read the record
// rather than the status code to know whether to exit non-zero.
func TestVmStartExitsNonZeroWhenTheOperationFailed(t *testing.T) {
	failure := "the unit did not become active"
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/vms/vm-one/start", func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, wire.Operation{
			OperationId: "cli-2", Verb: "start-vm", Uuid: "vm-one",
			Status: wire.OperationStatusFailure, Error: &failure,
		})
	})
	startFakeDaemon(t, mux)
	var output, errorOutput bytes.Buffer

	if code := run([]string{"vm", "start", "vm-one"}, &output, &errorOutput); code != exitFailure {
		t.Fatalf("got exit %d, want %d", code, exitFailure)
	}
	if !strings.Contains(errorOutput.String(), failure) {
		t.Errorf("the failure was not reported: %q", errorOutput.String())
	}
}

func TestVmStopSendsTheFlagsItWasGiven(t *testing.T) {
	var received wire.StopRequest
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/vms/vm-one/stop", func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Errorf("could not decode the request: %v", err)
		}
		writeJSON(t, writer, wire.Operation{OperationId: received.OperationId, Verb: "stop-vm", Status: wire.OperationStatusSuccess})
	})
	startFakeDaemon(t, mux)
	var output, errorOutput bytes.Buffer

	code := run([]string{"vm", "stop", "vm-one", "--graceful=false", "--stop-timeout-seconds", "45"}, &output, &errorOutput)

	if code != exitSuccess {
		t.Fatalf("got exit %d, want 0: %s", code, errorOutput.String())
	}
	if received.Graceful == nil || *received.Graceful {
		t.Errorf("got graceful %v, want false", received.Graceful)
	}
	if received.StopTimeoutSeconds == nil || *received.StopTimeoutSeconds != 45 {
		t.Errorf("got timeout %v, want 45", received.StopTimeoutSeconds)
	}
	if received.OperationId == "" {
		t.Error("the run was not journalled under any identifier")
	}
}

func TestVmShowAndHostFactsPrintTheirFields(t *testing.T) {
	sleeping := true
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms/vm-one", func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, wire.VirtualMachine{Uuid: "vm-one", ObservedStatus: wire.VirtualMachineStatusSleeping, Sleeping: &sleeping})
	})
	mux.HandleFunc("GET /v1/host", func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, wire.Host{Hostname: "atlas-host-1", BoatVersion: "0.1.0", VirtualMachineCount: 7})
	})
	startFakeDaemon(t, mux)

	var shown, errorOutput bytes.Buffer
	if code := run([]string{"vm", "show", "vm-one"}, &shown, &errorOutput); code != exitSuccess {
		t.Fatalf("vm show: got exit %d: %s", code, errorOutput.String())
	}
	for _, want := range []string{"vm-one", "Sleeping", "yes"} {
		if !strings.Contains(shown.String(), want) {
			t.Errorf("vm show does not mention %q: %s", want, shown.String())
		}
	}

	var facts bytes.Buffer
	if code := run([]string{"host", "facts"}, &facts, &errorOutput); code != exitSuccess {
		t.Fatalf("host facts: got exit %d: %s", code, errorOutput.String())
	}
	for _, want := range []string{"atlas-host-1", "0.1.0", "7"} {
		if !strings.Contains(facts.String(), want) {
			t.Errorf("host facts does not mention %q: %s", want, facts.String())
		}
	}
}

// The operator reads the daemon's sentence, not one the CLI made up.
func TestARefusalIsReportedInTheDaemonsWords(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/vms/ghost", func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
		writeJSON(t, writer, wire.Error{Error: "This host has not observed a virtual machine ghost."})
	})
	startFakeDaemon(t, mux)
	var output, errorOutput bytes.Buffer

	if code := run([]string{"vm", "show", "ghost"}, &output, &errorOutput); code != exitFailure {
		t.Fatalf("got exit %d, want %d", code, exitFailure)
	}
	if !strings.Contains(errorOutput.String(), "has not observed a virtual machine ghost") {
		t.Errorf("the daemon's sentence did not reach the operator: %q", errorOutput.String())
	}
}

func TestAnUnreachableDaemonSaysSo(t *testing.T) {
	t.Setenv("BOAT_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	var output, errorOutput bytes.Buffer

	if code := run([]string{"vm", "ls"}, &output, &errorOutput); code != exitFailure {
		t.Fatalf("got exit %d, want %d", code, exitFailure)
	}
	if !strings.Contains(errorOutput.String(), "could not reach the boat daemon") {
		t.Errorf("got %q, want a sentence about reaching the daemon", errorOutput.String())
	}
}
