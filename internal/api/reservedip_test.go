package api

import (
	"net/http"
	"testing"

	"github.com/frappe/boat/internal/netapply/reservedip"
	"github.com/frappe/boat/internal/vm"
	"github.com/frappe/boat/internal/wire"
)

func reservedIPv4Pointer(address string) *string { return &address }

// A valid attach runs the verb once with the canonicalised address and reports
// which delivery model the host used, so the Task shows anchor or routed.
func TestReservedIPAttachRunsTheVerbAndReportsTheDeliveryModel(t *testing.T) {
	machines := &fakeVirtualMachines{
		reservedDelivery: reservedip.Delivery{Anchored: true, Anchor: reservedip.Anchor{Address: "10.47.0.10"}},
	}
	server := newTestServer(newFakeStore(), machines)
	handler := server.SocketHandler()

	body := wire.ReservedIpRequest{
		OperationId:  "Task-rip-1",
		Action:       wire.ReservedIpRequestActionAttach,
		ReservedIpv4: reservedIPv4Pointer("146.190.11.153"),
	}
	recorder := postJSON(t, handler, "/vms/"+testUuid+"/reserved-ip", body)
	awaitOperation(t, server)

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	if len(machines.reservedIPRequests) != 1 || machines.reservedIPRequests[0] != (vm.ReservedIPRequest{ReservedIPv4: "146.190.11.153"}) {
		t.Fatalf("the verb saw %+v", machines.reservedIPRequests)
	}
	operation := decodeOperation(t, recorder)
	if operation.Status != wire.OperationStatusSuccess || operation.Verb != verbReservedIPVirtualMachine {
		t.Errorf("operation names the wrong work or status: %+v", operation)
	}
	if operation.Result == nil || (*operation.Result)["delivery"] != "anchor" {
		t.Errorf("the delivery model did not reach the result: %+v", operation.Result)
	}
}

// Detach runs the verb keyed on the guest — no reserved IP is needed and none is
// stated — and reports no result, because it removed a NAT and nothing else.
func TestReservedIPDetachRunsTheVerbWithNoAddress(t *testing.T) {
	machines := &fakeVirtualMachines{}
	server := newTestServer(newFakeStore(), machines)
	handler := server.SocketHandler()

	body := wire.ReservedIpRequest{OperationId: "Task-rip-2", Action: wire.ReservedIpRequestActionDetach}
	recorder := postJSON(t, handler, "/vms/"+testUuid+"/reserved-ip", body)

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
	if len(machines.reservedIPRequests) != 1 || !machines.reservedIPRequests[0].Detach {
		t.Fatalf("the verb saw %+v, want one detach", machines.reservedIPRequests)
	}
	if operation := recordOf(t, server, handler, "Task-rip-2"); operation.Result != nil {
		t.Errorf("a detach carried a result: %+v", operation.Result)
	}
}

// A reserved IP that is not an address is refused at the boundary, before the verb
// runs or an operation is claimed: it would otherwise be rendered into an nft rule.
func TestReservedIPAttachRefusesAValueThatIsNotAnAddress(t *testing.T) {
	for _, testCase := range []struct {
		name string
		body wire.ReservedIpRequest
	}{
		{
			"not an IPv4",
			wire.ReservedIpRequest{OperationId: "Task-rip-3", Action: wire.ReservedIpRequestActionAttach, ReservedIpv4: reservedIPv4Pointer("not-an-ip")},
		},
		{
			"an IPv6 on the v4 plane",
			wire.ReservedIpRequest{OperationId: "Task-rip-4", Action: wire.ReservedIpRequestActionAttach, ReservedIpv4: reservedIPv4Pointer("2001:db8::9")},
		},
		{
			"attach with no address at all",
			wire.ReservedIpRequest{OperationId: "Task-rip-5", Action: wire.ReservedIpRequestActionAttach},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			machines := &fakeVirtualMachines{}
			server := newTestServer(newFakeStore(), machines)
			handler := server.SocketHandler()

			recorder := postJSON(t, handler, "/vms/"+testUuid+"/reserved-ip", testCase.body)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400: %s", recorder.Code, recorder.Body)
			}
			if len(machines.reservedIPRequests) != 0 {
				t.Errorf("a refused request reached the host: %+v", machines.reservedIPRequests)
			}
		})
	}
}

// The whole request is still an operation identifier plus the reserved IP, so a
// missing identifier is refused exactly as it is for every other verb.
func TestReservedIPRefusesAMissingOperationIdentifier(t *testing.T) {
	machines := &fakeVirtualMachines{}
	server := newTestServer(newFakeStore(), machines)
	handler := server.SocketHandler()

	body := wire.ReservedIpRequest{Action: wire.ReservedIpRequestActionAttach, ReservedIpv4: reservedIPv4Pointer("146.190.11.153")}
	recorder := postJSON(t, handler, "/vms/"+testUuid+"/reserved-ip", body)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400: %s", recorder.Code, recorder.Body)
	}
	if len(machines.reservedIPRequests) != 0 {
		t.Errorf("a request with no identifier reached the host: %+v", machines.reservedIPRequests)
	}
}
