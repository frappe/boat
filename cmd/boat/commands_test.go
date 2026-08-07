package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
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

	if code := dispatch([]string{"vm", "ls"}, &output, &errorOutput); code != exitSuccess {
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

	if code := dispatch([]string{"vm", "start", "vm-one"}, &output, &errorOutput); code != exitSuccess {
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

	if code := dispatch([]string{"vm", "start", "vm-one"}, &output, &errorOutput); code != exitFailure {
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

	code := dispatch([]string{"vm", "stop", "vm-one", "--graceful=false", "--stop-timeout-seconds", "45"}, &output, &errorOutput)

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

func TestVmAdoptAssertsDesiredViaPut(t *testing.T) {
	var gotUUID string
	var gotBody wire.DesiredVirtualMachine
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/vms/{uuid}", func(writer http.ResponseWriter, request *http.Request) {
		gotUUID = request.PathValue("uuid")
		if err := json.NewDecoder(request.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writeJSON(t, writer, gotBody) // echo it back as the stored record
	})
	startFakeDaemon(t, mux)

	// Default power is Running, boot_epoch is 1.
	var output, errorOutput bytes.Buffer
	if code := dispatch([]string{"vm", "adopt", "vm-one"}, &output, &errorOutput); code != exitSuccess {
		t.Fatalf("got exit %d, want 0: %s", code, errorOutput.String())
	}
	if gotUUID != "vm-one" {
		t.Errorf("PUT reached uuid %q, want vm-one", gotUUID)
	}
	if gotBody.DesiredPower != wire.DesiredPowerRunning {
		t.Errorf("desired_power = %q, want Running", gotBody.DesiredPower)
	}
	if gotBody.BootEpoch != 1 {
		t.Errorf("boot_epoch = %d, want 1", gotBody.BootEpoch)
	}
	if !strings.Contains(output.String(), "adopted vm-one") {
		t.Errorf("output does not confirm the adoption: %s", output.String())
	}

	// --power stopped asserts a Stopped desire.
	output.Reset()
	errorOutput.Reset()
	if code := dispatch([]string{"vm", "adopt", "vm-one", "--power", "stopped"}, &output, &errorOutput); code != exitSuccess {
		t.Fatalf("got exit %d, want 0: %s", code, errorOutput.String())
	}
	if gotBody.DesiredPower != wire.DesiredPowerStopped {
		t.Errorf("desired_power = %q, want Stopped", gotBody.DesiredPower)
	}
}

func TestVmAdoptRejectsUnknownPower(t *testing.T) {
	called := false
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/vms/{uuid}", func(writer http.ResponseWriter, request *http.Request) {
		called = true
		writeJSON(t, writer, wire.DesiredVirtualMachine{})
	})
	startFakeDaemon(t, mux)
	var output, errorOutput bytes.Buffer
	if code := dispatch([]string{"vm", "adopt", "vm-one", "--power", "sideways"}, &output, &errorOutput); code != exitUsage {
		t.Errorf("got exit %d, want exitUsage for a bad --power", code)
	}
	if called {
		t.Error("a bad --power must be refused before any PUT is sent")
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
	if code := dispatch([]string{"vm", "show", "vm-one"}, &shown, &errorOutput); code != exitSuccess {
		t.Fatalf("vm show: got exit %d: %s", code, errorOutput.String())
	}
	for _, want := range []string{"vm-one", "Sleeping", "yes"} {
		if !strings.Contains(shown.String(), want) {
			t.Errorf("vm show does not mention %q: %s", want, shown.String())
		}
	}

	var facts bytes.Buffer
	if code := dispatch([]string{"host", "facts"}, &facts, &errorOutput); code != exitSuccess {
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

	if code := dispatch([]string{"vm", "show", "ghost"}, &output, &errorOutput); code != exitFailure {
		t.Fatalf("got exit %d, want %d", code, exitFailure)
	}
	if !strings.Contains(errorOutput.String(), "has not observed a virtual machine ghost") {
		t.Errorf("the daemon's sentence did not reach the operator: %q", errorOutput.String())
	}
}

func TestAnUnreachableDaemonSaysSo(t *testing.T) {
	t.Setenv("BOAT_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	var output, errorOutput bytes.Buffer

	if code := dispatch([]string{"vm", "ls"}, &output, &errorOutput); code != exitFailure {
		t.Fatalf("got exit %d, want %d", code, exitFailure)
	}
	if !strings.Contains(errorOutput.String(), "could not reach the boat daemon") {
		t.Errorf("got %q, want a sentence about reaching the daemon", errorOutput.String())
	}
}

// A verb's typed result is the one thing it decided that its trace does not
// spell out. An operator sleeping a VM from a shell has to see whether the
// guest's RAM was captured or the VM merely stopped, because that is the
// difference between a wake in milliseconds and a cold boot.
func TestASleepPrintsTheResultTheHostReported(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/vms/vm-one/sleep", func(writer http.ResponseWriter, request *http.Request) {
		result := map[string]any{"memory_snapshot": false, "reason": "not enough free space"}
		writeJSON(t, writer, wire.Operation{
			OperationId: "cli-sleep", Verb: "sleep-vm", Uuid: "vm-one",
			Status: wire.OperationStatusSuccess, Result: &result,
		})
	})
	startFakeDaemon(t, mux)
	var output, errorOutput bytes.Buffer

	if code := dispatch([]string{"vm", "sleep", "vm-one"}, &output, &errorOutput); code != exitSuccess {
		t.Fatalf("got exit %d, want 0: %s", code, errorOutput.String())
	}
	if !strings.Contains(output.String(), "not enough free space") {
		t.Errorf("the reason the next wake will be cold was not printed: %s", output.String())
	}
}

// Every verb the API serves must be reachable from the CLI, or the break-glass
// tool is weaker than the thing it exists to reach past. The verb name is the
// path segment, and this is what says so.
func TestEveryPlainVerbReachesItsOwnEndpoint(t *testing.T) {
	for _, verb := range []string{"pause", "resume", "sleep", "wake", "terminate", "resize"} {
		t.Run(verb, func(t *testing.T) {
			var asked string
			mux := http.NewServeMux()
			mux.HandleFunc("POST /v1/vms/vm-one/"+verb, func(writer http.ResponseWriter, request *http.Request) {
				var body wire.OperationRequest
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Errorf("could not read the request body: %v", err)
				}
				asked = body.OperationId
				writeJSON(t, writer, wire.Operation{
					OperationId: body.OperationId, Verb: verb + "-vm", Uuid: "vm-one",
					Status: wire.OperationStatusSuccess,
				})
			})
			startFakeDaemon(t, mux)
			var output, errorOutput bytes.Buffer

			if code := dispatch([]string{"vm", verb, "vm-one"}, &output, &errorOutput); code != exitSuccess {
				t.Fatalf("got exit %d, want 0: %s", code, errorOutput.String())
			}
			// Announced before the run, so an operator can read /ops/<id> back even
			// when the request never returns.
			if asked == "" || !strings.Contains(output.String(), asked) {
				t.Errorf("the operation identifier %q was not announced: %s", asked, output.String())
			}
		})
	}
}

func TestAVerbNeedsItsUuidFirst(t *testing.T) {
	startFakeDaemon(t, http.NewServeMux())
	var output, errorOutput bytes.Buffer

	if code := dispatch([]string{"vm", "sleep"}, &output, &errorOutput); code != exitUsage {
		t.Fatalf("got exit %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errorOutput.String(), "needs a UUID") {
		t.Errorf("got %q, want a sentence about the UUID", errorOutput.String())
	}
}

// A rebuild with no identity lays down a rootfs carrying no authorized keys, so
// the VM boots and nothing can ever reach it. Every observable signal says the
// rebuild worked, which is why this is refused here rather than left to care.
func TestRebuildRefusesWithoutAnIdentityFile(t *testing.T) {
	startFakeDaemon(t, http.NewServeMux())
	var output, errorOutput bytes.Buffer

	code := dispatch([]string{"vm", "rebuild", "vm-one", "--image", "ubuntu-24.04"}, &output, &errorOutput)
	if code != exitFailure {
		t.Fatalf("got exit %d, want %d", code, exitFailure)
	}
	if !strings.Contains(errorOutput.String(), "no authorized keys") {
		t.Errorf("got %q, want the sentence naming the consequence", errorOutput.String())
	}
}

func TestRebuildRefusesWithoutASource(t *testing.T) {
	startFakeDaemon(t, http.NewServeMux())
	var output, errorOutput bytes.Buffer

	identity := filepath.Join(t.TempDir(), "identity.json")
	if err := os.WriteFile(identity, []byte(`{"authorized_keys_blob":"ssh-ed25519 AAAA"}`), 0o600); err != nil {
		t.Fatalf("could not write the identity file: %v", err)
	}
	code := dispatch([]string{"vm", "rebuild", "vm-one", "--identity-file", identity}, &output, &errorOutput)
	if code != exitFailure {
		t.Fatalf("got exit %d, want %d", code, exitFailure)
	}
	if !strings.Contains(errorOutput.String(), "needs a source") {
		t.Errorf("got %q, want a sentence about the source", errorOutput.String())
	}
}

// A misspelled field is the same unreachable VM the required file prevents, so
// it is refused rather than dropped.
func TestRebuildRefusesAnIdentityFieldTheContractDoesNotHave(t *testing.T) {
	startFakeDaemon(t, http.NewServeMux())
	var output, errorOutput bytes.Buffer

	identity := filepath.Join(t.TempDir(), "identity.json")
	if err := os.WriteFile(identity, []byte(`{"authorised_keys_blob":"ssh-ed25519 AAAA"}`), 0o600); err != nil {
		t.Fatalf("could not write the identity file: %v", err)
	}
	code := dispatch([]string{
		"vm", "rebuild", "vm-one", "--image", "ubuntu-24.04", "--identity-file", identity,
	}, &output, &errorOutput)
	if code != exitFailure {
		t.Fatalf("got exit %d, want %d", code, exitFailure)
	}
	if !strings.Contains(errorOutput.String(), "authorised_keys_blob") {
		t.Errorf("got %q, want the refusal to name the field: %s", errorOutput.String(), output.String())
	}
}

func TestRebuildSendsTheSourceAndTheIdentityVerbatim(t *testing.T) {
	var received wire.RebuildRequest
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/vms/vm-one/rebuild", func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Errorf("could not read the request body: %v", err)
		}
		writeJSON(t, writer, wire.Operation{
			OperationId: received.OperationId, Verb: "rebuild-vm", Uuid: "vm-one",
			Status: wire.OperationStatusSuccess,
		})
	})
	startFakeDaemon(t, mux)
	var output, errorOutput bytes.Buffer

	identity := filepath.Join(t.TempDir(), "identity.json")
	document := `{"authorized_keys_blob":"ssh-ed25519 AAAA","ipv6_address":"2400:6180::1",` +
		`"extra_env":[{"path":"/etc/atlas-routing.env","content":"ATLAS_ROUTING_BASE_URL=x\n"}]}`
	if err := os.WriteFile(identity, []byte(document), 0o600); err != nil {
		t.Fatalf("could not write the identity file: %v", err)
	}
	code := dispatch([]string{
		"vm", "rebuild", "vm-one", "--image", "ubuntu-24.04", "--identity-file", identity,
	}, &output, &errorOutput)
	if code != exitSuccess {
		t.Fatalf("got exit %d, want 0: %s", code, errorOutput.String())
	}
	if received.Image == nil || *received.Image != "ubuntu-24.04" {
		t.Errorf("the image did not reach the daemon: %+v", received.Image)
	}
	if received.SnapshotDevice != nil {
		t.Errorf("an unset source was sent as %q rather than left out", *received.SnapshotDevice)
	}
	if received.Identity == nil || received.Identity.AuthorizedKeysBlob == nil ||
		*received.Identity.AuthorizedKeysBlob != "ssh-ed25519 AAAA" {
		t.Fatalf("the identity did not reach the daemon: %+v", received.Identity)
	}
	// Written through without being read: the CLI must not acquire an opinion
	// about a guest env file's meaning (spec/33 §7.2).
	if received.Identity.ExtraEnv == nil || len(*received.Identity.ExtraEnv) != 1 ||
		(*received.Identity.ExtraEnv)[0].Path != "/etc/atlas-routing.env" {
		t.Errorf("the extra env entries did not survive: %+v", received.Identity.ExtraEnv)
	}
}
