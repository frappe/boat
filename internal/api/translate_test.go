package api

import (
	"testing"

	"github.com/frappe/boat/internal/model"
)

// The Firecracker pid is absent rather than zero, because 0 is not a pid and a
// present zero would read as one. Every VM that is not running carries no pid,
// and so does the running VM whose socket holder `ss` could not name — the pid is
// a diagnostic and the liveness claim is the socket having answered, so an absent
// one says nothing about the status beside it.
func TestTheFirecrackerPidIsCarriedOnlyWhenThereIsOne(t *testing.T) {
	for name, testCase := range map[string]struct {
		pid  int
		want *int
	}{
		"a running VM":                   {15843, pointerTo(15843)},
		"nothing answered on the socket": {0, nil},
	} {
		t.Run(name, func(t *testing.T) {
			document := virtualMachineToWire(model.VirtualMachine{
				UUID:           testUuid,
				ObservedStatus: model.StatusRunning,
				FirecrackerPID: testCase.pid,
			})

			switch {
			case testCase.want == nil && document.FirecrackerPid != nil:
				t.Errorf("firecracker_pid = %d, want it absent", *document.FirecrackerPid)
			case testCase.want != nil && document.FirecrackerPid == nil:
				t.Errorf("firecracker_pid is absent, want %d", *testCase.want)
			case testCase.want != nil && *document.FirecrackerPid != *testCase.want:
				t.Errorf("firecracker_pid = %d, want %d", *document.FirecrackerPid, *testCase.want)
			}
		})
	}
}

func pointerTo(value int) *int { return &value }
