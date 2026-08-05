package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/vm"
	"github.com/frappe/boat/internal/wire"
)

// endpoint is one verb as the shared tests below drive it: where to post, what
// the record should be called, and how to count the times the host was really
// driven. Everything each verb does BEYOND this — reading desired state,
// sourcing a uid, refusing a stopped VM — is its own test further down.
type endpoint struct {
	path string
	verb string
	body func(identifier string) any
	ran  func(*fakeVirtualMachines) int
}

func operationBody(identifier string) any { return wire.OperationRequest{OperationId: identifier} }

// theVerbs is the whole WO-2 surface. A verb added to the contract and not to
// this map is a verb with no 404, no replay and no unreplayable-request test.
var theVerbs = map[string]endpoint{
	"pause": {
		path: "pause", verb: verbPauseVirtualMachine, body: operationBody,
		ran: func(fake *fakeVirtualMachines) int { return fake.pauses },
	},
	"resume": {
		path: "resume", verb: verbResumeVirtualMachine, body: operationBody,
		ran: func(fake *fakeVirtualMachines) int { return fake.resumes },
	},
	"sleep": {
		path: "sleep", verb: verbSleepVirtualMachine, body: operationBody,
		ran: func(fake *fakeVirtualMachines) int { return len(fake.sleepRequests) },
	},
	"wake": {
		path: "wake", verb: verbWakeVirtualMachine, body: operationBody,
		ran: func(fake *fakeVirtualMachines) int { return fake.wakes },
	},
	"terminate": {
		path: "terminate", verb: verbTerminateVirtualMachine, body: operationBody,
		ran: func(fake *fakeVirtualMachines) int { return fake.terminates },
	},
	"resize": {
		path: "resize", verb: verbResizeVirtualMachine, body: operationBody,
		ran: func(fake *fakeVirtualMachines) int { return len(fake.resizeRequests) },
	},
	"rebuild": {
		path: "rebuild", verb: verbRebuildVirtualMachine,
		body: func(identifier string) any {
			image := "ubuntu-24.04"
			return wire.RebuildRequest{OperationId: identifier, Image: &image}
		},
		ran: func(fake *fakeVirtualMachines) int { return len(fake.rebuildRequests) },
	},
}

// aFencedRunningVirtualMachine is the ordinary case every verb is entitled to
// assume: Atlas has asserted this VM's shape and this host holds its fence.
func aFencedRunningVirtualMachine() *fakeStore {
	operations := newFakeStore()
	operations.fence(testUuid, 1)
	operations.desire(model.DesiredVirtualMachine{
		UUID: testUuid, BootEpoch: 1, DesiredPower: model.PowerRunning,
		VCPUs: 2, CPUMaxCores: 2, CPUMode: "Hard cap",
		MemoryMegabytes: 1024, DiskGigabytes: 40, DataDiskGigabytes: 100,
	})
	return operations
}

func (verb endpoint) post(t *testing.T, handler http.Handler, identifier string) *httptest.ResponseRecorder {
	t.Helper()
	return postJSON(t, handler, "/vms/"+testUuid+"/"+verb.path, verb.body(identifier))
}

func TestEveryVerbRecordsItsOwnNameAndObservesTheHost(t *testing.T) {
	for name, verb := range theVerbs {
		operations := aFencedRunningVirtualMachine()
		machines := &fakeVirtualMachines{
			traceText: "+ " + verb.verb + "\n",
			observed:  model.VirtualMachine{ObservedStatus: model.StatusStopped, UnitActiveState: "inactive"},
		}
		server := newTestServer(operations, machines)
		handler := server.SocketHandler()

		recorder := verb.post(t, handler, "Task-"+name)

		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200: %s", name, recorder.Code, recorder.Body)
		}
		// The POST carries the claim; the outcome is the record, read as the
		// client reads it.
		operation := recordOf(t, server, handler, "Task-"+name)
		if operation.Status != wire.OperationStatusSuccess {
			t.Errorf("%s: got status %q, want Success", name, operation.Status)
		}
		if operation.Verb != verb.verb || operation.Uuid != testUuid {
			t.Errorf("%s: the operation names the wrong work: %+v", name, operation)
		}
		if verb.ran(machines) != 1 {
			t.Errorf("%s: the verb ran %d times, want 1", name, verb.ran(machines))
		}
		if operation.Output == nil || !strings.Contains(*operation.Output, verb.verb) {
			t.Errorf("%s: the trace did not reach Output: %v", name, operation.Output)
		}
		if operations.virtualMachines[testUuid].ObservedStatus != model.StatusStopped {
			t.Errorf("%s: the observation was not persisted", name)
		}
	}
}

// A retried Atlas Task carries the same name. Terminating twice or rebuilding
// twice because a response was lost is exactly what the journal exists to stop.
func TestEveryVerbReplaysInsteadOfRunningTwice(t *testing.T) {
	for name, verb := range theVerbs {
		operations := aFencedRunningVirtualMachine()
		machines := &fakeVirtualMachines{traceText: "+ " + verb.verb + "\n"}
		server := newTestServer(operations, machines)
		handler := server.SocketHandler()

		verb.post(t, handler, "Task-replay")
		first := recordOf(t, server, handler, "Task-replay")
		second := verb.post(t, handler, "Task-replay")

		if second.Code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200: %s", name, second.Code, second.Body)
		}
		if verb.ran(machines) != 1 {
			t.Errorf("%s: the verb ran %d times, want 1", name, verb.ran(machines))
		}
		replayed := decodeOperation(t, second)
		if replayed.Status != first.Status || *replayed.Output != *first.Output {
			t.Errorf("%s: the replay differs from the first result: %+v vs %+v", name, replayed, first)
		}
	}
}

// A verb for a VM this host does not have is a 404 — and still a terminal
// journal record, because the operation was claimed before anyone knew that.
func TestEveryVerbRefusesAVirtualMachineThisHostDoesNotHave(t *testing.T) {
	for name, verb := range theVerbs {
		operations := aFencedRunningVirtualMachine()
		machines := &fakeVirtualMachines{missing: true}
		server := newTestServer(operations, machines)
		handler := server.SocketHandler()

		recorder := verb.post(t, handler, "Task-absent")

		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s: got %d, want 404: %s", name, recorder.Code, recorder.Body)
		}
		if verb.ran(machines) != 0 {
			t.Errorf("%s: a missing VM was driven anyway", name)
		}
		if recorded := operations.operations["Task-absent"]; recorded.Status != model.OperationFailure {
			t.Errorf("%s: the refusal left no terminal record: %+v", name, recorded)
		}
	}
}

func TestEveryVerbNeedsAnOperationIdentifier(t *testing.T) {
	for name, verb := range theVerbs {
		operations := aFencedRunningVirtualMachine()
		machines := &fakeVirtualMachines{}
		server := newTestServer(operations, machines)
		handler := server.SocketHandler()

		recorder := verb.post(t, handler, "")

		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400: %s", name, recorder.Code, recorder.Body)
		}
		if verb.ran(machines) != 0 {
			t.Errorf("%s: an unreplayable request drove the host", name)
		}
	}
}

func TestEveryVerbRefusesAnIdentifierAlreadyUsedForOtherWork(t *testing.T) {
	for name, verb := range theVerbs {
		operations := aFencedRunningVirtualMachine()
		machines := &fakeVirtualMachines{}
		server := newTestServer(operations, machines)
		handler := server.SocketHandler()
		postJSON(t, handler, "/vms/"+testUuid+"/stop", wire.StopRequest{OperationId: "Task-taken"})

		recorder := verb.post(t, handler, "Task-taken")

		if recorder.Code != http.StatusConflict {
			t.Fatalf("%s: got %d, want 409: %s", name, recorder.Code, recorder.Body)
		}
		if verb.ran(machines) != 0 {
			t.Errorf("%s: a conflicting claim drove the host", name)
		}
	}
}

// §11.3, and the reason the whole sleep reflex is safe: an operator's stop
// outranks anything that asks for the VM back. The verbs that would resurrect
// it refuse rather than obey, and say what to assert instead.
func TestWakeAndResumeDoNotOutrankAStoppedDesire(t *testing.T) {
	for _, path := range []string{"wake", "resume"} {
		operations := aFencedRunningVirtualMachine()
		operations.desire(model.DesiredVirtualMachine{
			UUID: testUuid, BootEpoch: 1, DesiredPower: model.PowerStopped,
		})
		machines := &fakeVirtualMachines{}
		server := newTestServer(operations, machines)
		handler := server.SocketHandler()

		recorder := postJSON(t, handler, "/vms/"+testUuid+"/"+path, wire.OperationRequest{OperationId: "Task-9"})

		if recorder.Code != http.StatusConflict {
			t.Fatalf("%s: got %d, want 409: %s", path, recorder.Code, recorder.Body)
		}
		if !strings.Contains(decodeError(t, recorder).Error, "desired_power=Running") {
			t.Errorf("%s: the refusal does not say what to assert: %q", path, decodeError(t, recorder).Error)
		}
		if machines.wakes != 0 || machines.resumes != 0 {
			t.Errorf("%s: a stopped VM was brought back anyway", path)
		}
	}
}

// The refusal must not consume the identifier, because the retry that matters
// is the one right after Atlas asserts Running — under the same Task name.
func TestARefusedWakeLeavesItsIdentifierReusable(t *testing.T) {
	operations := aFencedRunningVirtualMachine()
	operations.desire(model.DesiredVirtualMachine{UUID: testUuid, BootEpoch: 1, DesiredPower: model.PowerStopped})
	machines := &fakeVirtualMachines{}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()
	postJSON(t, handler, "/vms/"+testUuid+"/wake", wire.OperationRequest{OperationId: "Task-10"})

	operations.desire(model.DesiredVirtualMachine{UUID: testUuid, BootEpoch: 1, DesiredPower: model.PowerRunning})
	recorder := postJSON(t, handler, "/vms/"+testUuid+"/wake", wire.OperationRequest{OperationId: "Task-10"})

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	if machines.wakes != 1 {
		t.Errorf("the VM was woken %d times, want 1", machines.wakes)
	}
}

// Waking is booting, so the fence gates it exactly as it gates a start: the VM
// whose artifacts are on this disk may already be live on the host it moved to.
func TestWakeIsFencedLikeAStart(t *testing.T) {
	operations := newFakeStore()
	operations.desire(model.DesiredVirtualMachine{UUID: testUuid, BootEpoch: 1, DesiredPower: model.PowerRunning})
	machines := &fakeVirtualMachines{}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/wake", wire.OperationRequest{OperationId: "Task-11"})

	if recorder.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409: %s", recorder.Code, recorder.Body)
	}
	if machines.wakes != 0 {
		t.Error("an unfenced VM was woken")
	}
}

// A VM this host holds no desired state for is REFUSED, and this test used to
// assert the opposite.
//
// The old reasoning was that there is no assertion to outrank and refusing would
// leave an operator with a VM nothing could wake. That was defensible while the
// only way to reach "fence held, no desired record" was a crash between assert's
// two writes — which Atlas heals on its own, because every verb PUTs first.
//
// Retraction made the same state mean something else: this host has been told to
// stop holding intent for a VM another host may now own. A keep-address repoint
// leaves the tree in place, so Exists passes, and the old behaviour started the
// guest — two live copies of one VM on one disk, which is the failure the fence
// exists for. Refusing costs an operator one PUT; allowing costs a split brain.
func TestWakeIsRefusedWhenThisHostHoldsNoDesiredState(t *testing.T) {
	operations := newFakeStore()
	operations.fenceWithoutDesire(testUuid, 1)
	machines := &fakeVirtualMachines{}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/wake", wire.OperationRequest{OperationId: "Task-12"})

	if recorder.Code == http.StatusOK {
		t.Fatalf("a retracted VM was woken: %d %s", recorder.Code, recorder.Body)
	}
	if machines.wakes != 0 {
		t.Errorf("the VM was woken %d times, want 0", machines.wakes)
	}
}

// The sleep carries no uid on the wire. It comes off the host, because the
// sidecar is what the jail was actually built from.
func TestSleepSourcesTheFirecrackerUIDFromTheHost(t *testing.T) {
	operations := aFencedRunningVirtualMachine()
	machines := &fakeVirtualMachines{firecrackerUID: 247312}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()

	postJSON(t, handler, "/vms/"+testUuid+"/sleep", wire.OperationRequest{OperationId: "Task-13"})

	if len(machines.sleepRequests) != 1 {
		t.Fatalf("the verb ran %d times, want 1", len(machines.sleepRequests))
	}
	if machines.sleepRequests[0].FirecrackerUID != 247312 {
		t.Errorf("the uid did not reach the verb: %+v", machines.sleepRequests[0])
	}
}

// Uid 0 is root, and a snapshot directory owned by root is a snapshot that is
// never written — a failure the guest only reveals on the next wake. So a uid
// that cannot be read fails the sleep instead of defaulting.
func TestSleepFailsWhenTheFirecrackerUIDCannotBeRead(t *testing.T) {
	operations := aFencedRunningVirtualMachine()
	machines := &fakeVirtualMachines{firecrackerUIDErr: errors.New("network.env names no ATLAS_FC_UID")}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/sleep", wire.OperationRequest{OperationId: "Task-14"})

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	if operation := decodeOperation(t, recorder); operation.Status != wire.OperationStatusFailure {
		t.Errorf("got status %q, want Failure", operation.Status)
	}
	if len(machines.sleepRequests) != 0 {
		t.Error("the VM was slept with a uid nobody could read")
	}
}

// The resize applies the shape Atlas asserted, and derives the jailer's caps
// from the same numbers. The cgroup values are pinned against the Python's in
// internal/vm; what is pinned here is that they are derived at all — a resize
// that moved the guest's RAM and left memory.max behind is the OOM-kill.
func TestResizeAppliesTheDesiredShapeAndItsCgroupCaps(t *testing.T) {
	operations := aFencedRunningVirtualMachine()
	machines := &fakeVirtualMachines{}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()

	postJSON(t, handler, "/vms/"+testUuid+"/resize", wire.OperationRequest{OperationId: "Task-15"})

	if len(machines.resizeRequests) != 1 {
		t.Fatalf("the verb ran %d times, want 1", len(machines.resizeRequests))
	}
	resize := machines.resizeRequests[0]
	want := vm.ResizeRequest{
		VCPUs: 2, MemoryMB: 1024, DiskGB: 40, DataDiskGB: 100,
		CgroupArguments: []string{"memory.max=1342177280", "memory.swap.max=0", "cpu.max=200000 100000"},
	}
	if resize.VCPUs != want.VCPUs || resize.MemoryMB != want.MemoryMB ||
		resize.DiskGB != want.DiskGB || resize.DataDiskGB != want.DataDiskGB {
		t.Errorf("the desired numbers did not reach the verb: %+v", resize)
	}
	if !slices.Equal(resize.CgroupArguments, want.CgroupArguments) {
		t.Errorf("cgroup values:\ngot:  %q\nwant: %q", resize.CgroupArguments, want.CgroupArguments)
	}
	if resize.DataDiskFormatted {
		t.Error("the data disk's filesystem was grown on a flag nothing asserted")
	}
}

// Nothing asserted is nothing to apply. Running the verb anyway would write a
// machine config of no vCPU and no memory, and Firecracker reads it only at
// boot — so the resize would succeed now and the VM would fail to start later.
func TestResizeRefusesWhenThereIsNothingAssertedToApply(t *testing.T) {
	for name, desired := range map[string]*model.DesiredVirtualMachine{
		"nothing asserted": nil,
		"no numbers in it": {UUID: testUuid, BootEpoch: 1, DesiredPower: model.PowerRunning},
	} {
		operations := newFakeStore()
		operations.fence(testUuid, 1)
		if desired != nil {
			operations.desire(*desired)
		}
		machines := &fakeVirtualMachines{}
		server := newTestServer(operations, machines)
		handler := server.SocketHandler()

		recorder := postJSON(t, handler, "/vms/"+testUuid+"/resize", wire.OperationRequest{OperationId: "Task-16"})

		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400: %s", name, recorder.Code, recorder.Body)
		}
		if len(machines.resizeRequests) != 0 {
			t.Errorf("%s: the VM was resized to nothing", name)
		}
	}
}

// The rebuild's own inputs reach the verb; the sizes come from desired state
// and the uid from the host, so neither is on the wire to be sent stale.
func TestRebuildJoinsItsRequestToDesiredStateAndTheHost(t *testing.T) {
	operations := aFencedRunningVirtualMachine()
	machines := &fakeVirtualMachines{firecrackerUID: 247312}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()
	snapshot, dataSnapshot := "/dev/atlas/atlas-snap-77777777", "/dev/atlas/atlas-datasnap-99999999"

	postJSON(t, handler, "/vms/"+testUuid+"/rebuild", wire.RebuildRequest{
		OperationId:        "Task-17",
		SnapshotDevice:     &snapshot,
		DataSnapshotDevice: &dataSnapshot,
	})

	if len(machines.rebuildRequests) != 1 {
		t.Fatalf("the verb ran %d times, want 1", len(machines.rebuildRequests))
	}
	rebuild := machines.rebuildRequests[0]
	if rebuild.SnapshotDevice != snapshot || rebuild.DataSnapshotDevice != dataSnapshot {
		t.Errorf("the source did not reach the verb: %+v", rebuild)
	}
	if rebuild.DiskGB != 40 || rebuild.DataDiskGB != 100 {
		t.Errorf("the sizes did not come from desired state: %+v", rebuild)
	}
	if rebuild.FirecrackerUID != 247312 {
		t.Errorf("the uid did not come from the host: %+v", rebuild)
	}
}

// Rebuild is the one verb among the nine that makes a choice its own retry
// could not repeat, and the choice is written down BEFORE the verb runs: the
// rebuild's first act is to drop the VM's root volume, and after that the host
// holds no record of what it was supposed to become.
func TestRebuildRecordsItsSourceBeforeItLaysAnythingDown(t *testing.T) {
	operations := aFencedRunningVirtualMachine()
	machines := &fakeVirtualMachines{}
	snapshot := "/dev/atlas/atlas-snap-77777777"
	// Asserted from inside the verb, because "before" is the whole claim. A
	// decision recorded afterwards is a log, and a log cannot answer the one
	// question a replay has.
	machines.beforeRebuild = func() {
		if len(operations.decided()) != 1 {
			t.Errorf("the rebuild reached the host with %d decisions recorded, want its source first",
				len(operations.decided()))
		}
	}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()

	postJSON(t, handler, "/vms/"+testUuid+"/rebuild",
		wire.RebuildRequest{OperationId: "Task-21", SnapshotDevice: &snapshot})

	decisions := operations.decided()
	if len(decisions) != 1 {
		t.Fatalf("recorded %d decisions, want the one source this rebuild was authorized to use", len(decisions))
	}
	if decisions[0].OperationID != "Task-21" || decisions[0].Step != "rebuild-source" {
		t.Fatalf("recorded %+v, want the source under the operation that chose it", decisions[0])
	}
	if decisions[0].Values["snapshot_device"] != snapshot {
		t.Fatalf("recorded %v, want the device the request named", decisions[0].Values)
	}
}

// A decision that could not be written down is not a decision, and the verb must
// not run past it. Acting on a choice no crash could recover is the whole
// failure write-ahead journalling exists to prevent.
//
// The two cases are the same rule from either side: a journal that refused the
// write, and a Server that was built without a journal at all.
func TestRebuildDoesNotRunWhenItsSourceCannotBeRecorded(t *testing.T) {
	image := "ubuntu-24.04"
	cases := map[string]func(*fakeStore, *fakeVirtualMachines) *Server{
		"the journal refused": func(operations *fakeStore, machines *fakeVirtualMachines) *Server {
			operations.decisionError = errors.New("the store refused")
			return newTestServer(operations, machines)
		},
		"there is no journal": func(operations *fakeStore, machines *fakeVirtualMachines) *Server {
			return NewServer(Dependencies{Operations: operations, State: operations, VirtualMachines: machines})
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			operations, machines := aFencedRunningVirtualMachine(), &fakeVirtualMachines{}
			handler := build(operations, machines).SocketHandler()

			postJSON(t, handler, "/vms/"+testUuid+"/rebuild",
				wire.RebuildRequest{OperationId: "Task-22", Image: &image})

			if len(machines.rebuildRequests) != 0 {
				t.Fatalf("the rebuild ran %d times without its source recorded", len(machines.rebuildRequests))
			}
			if got := operations.operations["Task-22"].Status; got != model.OperationFailure {
				t.Fatalf("the operation was recorded %q, want the failure that stopped it", got)
			}
		})
	}
}

// The guest identity crosses the boundary as bytes. Boat carries an
// authorized-keys blob and a list of {path, content} files it cannot tell apart
// from one another, and nothing here knows what any of them are for.
func TestRebuildCarriesGuestIdentityAcrossAsOpaqueBytes(t *testing.T) {
	operations := aFencedRunningVirtualMachine()
	machines := &fakeVirtualMachines{}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()
	image, keys, address := "ubuntu-24.04", "ssh-ed25519 AAAAC3Nz owner\nssh-ed25519 AAAAC3Nz satellite", "2604:a880::1"
	mountAt := "/data"

	postJSON(t, handler, "/vms/"+testUuid+"/rebuild", wire.RebuildRequest{
		OperationId: "Task-18",
		Image:       &image,
		Identity: &wire.GuestIdentity{
			Ipv6Address:        &address,
			AuthorizedKeysBlob: &keys,
			DataDiskMountAt:    &mountAt,
			ExtraEnv: &[]wire.GuestFile{
				{Path: "/etc/atlas-routing.env", Content: "ROUTING_BASE_URL=https://orchestrator.blr1.frappe.dev\n"},
			},
		},
	})

	if len(machines.rebuildRequests) != 1 {
		t.Fatalf("the verb ran %d times, want 1", len(machines.rebuildRequests))
	}
	identity := machines.rebuildRequests[0].Identity
	if identity.AuthorizedKeys != keys {
		t.Errorf("the authorized keys were not carried whole: %q", identity.AuthorizedKeys)
	}
	if identity.IPv6Address != address || identity.DataDiskMountAt != mountAt {
		t.Errorf("the identity did not reach the verb: %+v", identity)
	}
	want := []vm.EnvironmentFile{{
		Path:    "/etc/atlas-routing.env",
		Content: "ROUTING_BASE_URL=https://orchestrator.blr1.frappe.dev\n",
	}}
	if !slices.Equal(identity.ExtraEnvironment, want) {
		t.Errorf("the guest files were not carried verbatim:\ngot:  %+v\nwant: %+v", identity.ExtraEnvironment, want)
	}
}

// A rebuild with no identity at all is legal — the verb writes the addresses it
// was given, and it was given none. It must not be a nil dereference.
func TestRebuildWithoutAnIdentityIsAnEmptyIdentity(t *testing.T) {
	operations := aFencedRunningVirtualMachine()
	machines := &fakeVirtualMachines{}
	server := newTestServer(operations, machines)
	handler := server.SocketHandler()
	image := "ubuntu-24.04"

	recorder := postJSON(t, handler, "/vms/"+testUuid+"/rebuild",
		wire.RebuildRequest{OperationId: "Task-19", Image: &image})

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	identity := machines.rebuildRequests[0].Identity
	if identity.AuthorizedKeys != "" || identity.IPv6Address != "" || identity.ExtraEnvironment != nil {
		t.Errorf("an absent identity became something: %+v", identity)
	}
}
