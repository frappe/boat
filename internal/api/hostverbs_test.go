// The host-verb endpoint: the transport that took the operations Atlas used to
// drive as `boat <verb>` over SSH — provision, snapshot, sync-image, per-VM
// networking — onto the same journaled HTTP surface every lifecycle verb uses.
// What these tests hold is that a host verb is claimed once, journals its typed
// result, mirrors its own exit code, and is refused at the boundary when the verb
// is one this host does not serve.

package api

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/wire"
)

// fakeHostVerbs stands in for cmd/boat's runner: it records what it was asked to
// run and answers with a scripted exit code, stdout (an ATLAS_RESULT= line) and
// stderr (trace), so a handler test needs no host under it.
type fakeHostVerbs struct {
	served    map[string]bool
	reads     map[string]bool
	runs      []string
	arguments []string
	stdout    string
	trace     string
	exitCode  int
}

func (fake *fakeHostVerbs) Serves(verb string) bool     { return fake.served[verb] }
func (fake *fakeHostVerbs) ServesRead(verb string) bool { return fake.reads[verb] }

func (fake *fakeHostVerbs) Run(verb string, arguments []string, stdout, stderr io.Writer) int {
	fake.runs = append(fake.runs, verb)
	fake.arguments = arguments
	if fake.trace != "" {
		fmt.Fprintln(stderr, fake.trace)
	}
	if fake.stdout != "" {
		fmt.Fprint(stdout, fake.stdout)
	}
	return fake.exitCode
}

func hostVerbServer(t *testing.T, verbs *fakeHostVerbs) (*Server, http.Handler) {
	t.Helper()
	server := newTestServer(newFakeStore(), &fakeVirtualMachines{})
	server.hostVerbs = verbs
	return server, server.SocketHandler()
}

func hostVerbBody(operationID string, variables map[string]any) wire.HostVerbRequest {
	return wire.HostVerbRequest{OperationId: operationID, Variables: &variables}
}

// A VM-scoped host verb runs, journals its typed result, and renders its
// variables to the flag argv the verb parses — the whole of the transport in one
// path.
func TestHostVerbRunsAndJournalsItsResult(t *testing.T) {
	verbs := &fakeHostVerbs{
		served: map[string]bool{"snapshot-vm": true},
		stdout: `ATLAS_RESULT={"size_bytes":123,"data_size_bytes":0}` + "\n",
	}
	server, handler := hostVerbServer(t, verbs)

	recorder := postJSON(t, handler, "/host-verbs/snapshot-vm", hostVerbBody("Task-snap-1", map[string]any{
		"VIRTUAL_MACHINE_NAME": testUuid,
		"SNAPSHOT_ROOTFS_PATH": "/dev/atlas/vm",
	}))

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	if len(verbs.runs) != 1 || verbs.runs[0] != "snapshot-vm" {
		t.Fatalf("ran %v, want one snapshot-vm", verbs.runs)
	}
	if !argvHasFlag(verbs.arguments, "--snapshot-rootfs-path", "/dev/atlas/vm") {
		t.Errorf("argv %v did not carry the rendered flag", verbs.arguments)
	}
	result := resultOf(t, recordOf(t, server, handler, "Task-snap-1"))
	if size, ok := result["size_bytes"].(float64); !ok || int64(size) != 123 {
		t.Errorf("size_bytes = %v, want 123", result["size_bytes"])
	}
}

// A host-scoped verb names no VM, so it is journaled with an empty uuid and never
// takes a per-VM turn.
func TestHostScopedVerbNeedsNoVirtualMachine(t *testing.T) {
	verbs := &fakeHostVerbs{served: map[string]bool{"sync-image": true}}
	server, handler := hostVerbServer(t, verbs)

	recorder := postJSON(t, handler, "/host-verbs/sync-image", hostVerbBody("Task-sync-1", map[string]any{
		"IMAGE_NAME": "bench-v16",
	}))

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	operation := recordOf(t, server, handler, "Task-sync-1")
	if operation.Status != wire.OperationStatusSuccess {
		t.Fatalf("status = %q, want Success", operation.Status)
	}
	if operation.Uuid != "" {
		t.Errorf("a host-scoped verb recorded uuid %q, want none", operation.Uuid)
	}
}

// A verb this host does not serve is refused at the boundary, BEFORE a claim — so
// the operation identifier is free to reuse and nothing is journaled.
func TestUnknownHostVerbIsRefusedBeforeClaim(t *testing.T) {
	verbs := &fakeHostVerbs{served: map[string]bool{"snapshot-vm": true}}
	server, handler := hostVerbServer(t, verbs)

	recorder := postJSON(t, handler, "/host-verbs/rm-rf-slash", hostVerbBody("Task-evil-1", nil))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", recorder.Code)
	}
	if len(verbs.runs) != 0 {
		t.Errorf("an unserved verb ran %v", verbs.runs)
	}
	awaitOperation(t, server)
	if get(t, handler, "/ops/Task-evil-1").Code != http.StatusNotFound {
		t.Errorf("an unserved verb left an operation record behind")
	}
}

// A verb needs an operation_id or a retry could not be told from a second run.
func TestHostVerbNeedsAnOperationIdentifier(t *testing.T) {
	verbs := &fakeHostVerbs{served: map[string]bool{"snapshot-vm": true}}
	_, handler := hostVerbServer(t, verbs)

	recorder := postJSON(t, handler, "/host-verbs/snapshot-vm", wire.HostVerbRequest{})

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", recorder.Code)
	}
}

// A failed host verb records its own exit code and the sentence it ended on, so
// the operation record reads like the Task it replaced.
func TestFailedHostVerbRecordsItsExitCode(t *testing.T) {
	verbs := &fakeHostVerbs{
		served:   map[string]bool{"snapshot-vm": true},
		trace:    "lvcreate: Volume group \"atlas\" has insufficient free space",
		exitCode: 5,
	}
	server, handler := hostVerbServer(t, verbs)

	postJSON(t, handler, "/host-verbs/snapshot-vm", hostVerbBody("Task-snap-fail", map[string]any{
		"VIRTUAL_MACHINE_NAME": testUuid,
	}))

	operation := recordOf(t, server, handler, "Task-snap-fail")
	if operation.Status != wire.OperationStatusFailure {
		t.Fatalf("status = %q, want Failure", operation.Status)
	}
	if operation.ExitCode == nil || *operation.ExitCode != 5 {
		t.Errorf("exit_code = %v, want the verb's own 5", operation.ExitCode)
	}
	if operation.Result != nil {
		t.Errorf("a failed verb reported a result: %v", *operation.Result)
	}
}

// The op_id is the replay key: re-posting one that already ran returns its first
// record and runs the verb no second time.
func TestHostVerbReplayDoesNotRunTwice(t *testing.T) {
	verbs := &fakeHostVerbs{
		served: map[string]bool{"snapshot-vm": true},
		stdout: `ATLAS_RESULT={"size_bytes":7}` + "\n",
	}
	server, handler := hostVerbServer(t, verbs)
	body := hostVerbBody("Task-snap-replay", map[string]any{"VIRTUAL_MACHINE_NAME": testUuid})

	postJSON(t, handler, "/host-verbs/snapshot-vm", body)
	awaitOperation(t, server)
	postJSON(t, handler, "/host-verbs/snapshot-vm", body)

	if len(verbs.runs) != 1 {
		t.Fatalf("the verb ran %d times, want once", len(verbs.runs))
	}
}

// A malformed UUID on a VM-scoped verb is refused before the runner is reached.
func TestHostVerbRefusesAMalformedUuid(t *testing.T) {
	verbs := &fakeHostVerbs{served: map[string]bool{"snapshot-vm": true}}
	_, handler := hostVerbServer(t, verbs)

	recorder := postJSON(t, handler, "/host-verbs/snapshot-vm", hostVerbBody("Task-bad-uuid", map[string]any{
		"VIRTUAL_MACHINE_NAME": "not-a-uuid",
	}))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", recorder.Code)
	}
	if len(verbs.runs) != 0 {
		t.Errorf("a verb ran against a malformed uuid: %v", verbs.runs)
	}
}

// A read verb answers with its output and writes NO operation record — the whole
// reason the per-minute sweeps do not bury the journal.
func TestHostReadReturnsOutputAndJournalsNothing(t *testing.T) {
	verbs := &fakeHostVerbs{
		reads:  map[string]bool{"poll-vm-traffic": true},
		stdout: `ATLAS_RESULT={"counters":{}}` + "\n",
	}
	server, handler := hostVerbServer(t, verbs)

	recorder := postJSON(t, handler, "/host-reads/poll-vm-traffic", wire.HostReadRequest{
		Variables: &map[string]any{"VMS_JSON": "[]"},
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	var result wire.HostReadResult
	decode(t, recorder, &result)
	if !strings.Contains(result.Output, "ATLAS_RESULT=") {
		t.Errorf("output %q carried no result line", result.Output)
	}
	// No journal: the operation store holds nothing for a read.
	awaitOperation(t, server)
	if got := get(t, handler, "/ops/poll-vm-traffic").Code; got != http.StatusNotFound {
		t.Errorf("a read left an operation record (GET /ops -> %d)", got)
	}
}

// A mutating verb is not a read: /host-reads refuses it even though the daemon
// serves it over /host-verbs.
func TestHostReadRefusesAMutatingVerb(t *testing.T) {
	verbs := &fakeHostVerbs{served: map[string]bool{"snapshot-vm": true}}
	_, handler := hostVerbServer(t, verbs)

	recorder := postJSON(t, handler, "/host-reads/snapshot-vm", wire.HostReadRequest{})

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", recorder.Code)
	}
}

func argvHasFlag(arguments []string, flag, value string) bool {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == flag && arguments[index+1] == value {
			return true
		}
	}
	return false
}
