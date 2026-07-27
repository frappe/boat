// Whole scans, driven through canned command output. Each scenario is a state a
// real host reaches: two VMs serving traffic, a VM stopped, a VM parked, a host
// that will not answer.

package adopt

import (
	"errors"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/run"
)

// The seams have exactly one real implementation each, and a nil one would take
// the daemon down on the first scan of a live host rather than in a test.
func TestNewScannerIsWiredToTheHost(t *testing.T) {
	scanner := NewScanner()
	runner := run.NewRunner(nil)

	if scanner.commandsFor == nil || scanner.observer == nil || scanner.clock == nil {
		t.Fatal("NewScanner left a seam nil")
	}
	if scanner.commandsFor(runner) != commands(runner) {
		t.Error("commandsFor did not hand back the runner it was given")
	}
	if scanner.clock.Now().IsZero() {
		t.Error("clock.Now() is zero")
	}
}

func TestScanReadsACleanHostWithTwoRunningVirtualMachines(t *testing.T) {
	host := newFakeHost().withRunning(firstUUID).withRunning(secondUUID)

	result, err := host.scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertAdopted(t, result, firstUUID, secondUUID)
	assertNotQuarantined(t, result)
	if result.VirtualMachines[0].ObservedStatus != model.StatusRunning {
		t.Errorf("status = %q, want %q", result.VirtualMachines[0].ObservedStatus, model.StatusRunning)
	}
}

// A stopped VM keeps every durable artifact and none of the runtime ones, and
// systemd has commonly unloaded its unit. All three are normal, so it is adopted.
func TestScanAdoptsAStoppedVirtualMachine(t *testing.T) {
	host := newFakeHost().withStopped(firstUUID)

	result, err := host.scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertAdopted(t, result, firstUUID)
	assertNotQuarantined(t, result)
	if result.VirtualMachines[0].ObservedStatus != model.StatusStopped {
		t.Errorf("status = %q, want %q", result.VirtualMachines[0].ObservedStatus, model.StatusStopped)
	}
}

// A sleeping VM's unit is inactive and its namespace is gone, but the host still
// answers proxy-NDP for it so an inbound SYN can wake it. That is a VM, and the
// status comes from internal/vm's marker read rather than from anything here.
func TestScanAdoptsASleepingVirtualMachine(t *testing.T) {
	host := newFakeHost().withSleeping(firstUUID)

	result, err := host.scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertAdopted(t, result, firstUUID)
	assertNotQuarantined(t, result)
	if result.VirtualMachines[0].ObservedStatus != model.StatusSleeping {
		t.Errorf("status = %q, want %q", result.VirtualMachines[0].ObservedStatus, model.StatusSleeping)
	}
}

func TestScanOfAnEmptyHostFindsNothing(t *testing.T) {
	result, err := newFakeHost().scan(t)

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	assertAdopted(t, result)
	assertNotQuarantined(t, result)
	if len(result.LogicalVolumes) != 1 || result.LogicalVolumes[0].Name != "pool0" {
		t.Errorf("volumes = %v, want just the thin pool", result.LogicalVolumes)
	}
}

func TestScanReportsTheHostInventoriesItReadFrom(t *testing.T) {
	host := newFakeHost().withRunning(firstUUID)

	result, _ := host.scan(t)

	if len(result.Units) != 1 || result.Units[0].ActiveState != "active" ||
		result.Units[0].SubState != "running" {
		t.Errorf("units = %+v, want one active/running instance", result.Units)
	}
	volume := result.LogicalVolumes[1]
	if volume.Name != "atlas-vm-"+firstUUID || volume.SizeBytes != 10737418240 ||
		volume.Pool != "pool0" || volume.Origin != "atlas-image-bench" {
		t.Errorf("volume = %+v, want the VM's thin snapshot with its origin", volume)
	}
}

// A scan that dropped one enumeration would report a host holding fewer VMs than
// it holds, and an empty host is indistinguishable from a wiped one.
func TestScanFailsWhenAnEnumerationFails(t *testing.T) {
	host := newFakeHost().withRunning(firstUUID)
	host.failing[listNamespaces] = true

	result, err := host.scan(t)

	if err == nil {
		t.Fatal("Scan succeeded, want the failed enumeration reported")
	}
	if !errors.Is(err, errCommandFailed) {
		t.Errorf("error = %v, want it to wrap the command failure", err)
	}
	if len(result.VirtualMachines) > 0 || len(result.Quarantined) > 0 || len(result.Units) > 0 {
		t.Errorf("failed scan returned a partial result: %+v", result)
	}
}

func TestScanFailsWhenAVirtualMachineCannotBeObserved(t *testing.T) {
	host := newFakeHost().withRunning(firstUUID)
	host.observeFail[firstUUID] = true

	result, err := host.scan(t)

	if err == nil {
		t.Fatal("Scan succeeded, want the failed observation reported")
	}
	if !strings.Contains(err.Error(), firstUUID) {
		t.Errorf("error = %v, want it to name the VM it could not read", err)
	}
	if len(result.VirtualMachines) > 0 {
		t.Errorf("failed scan returned VMs: %+v", result.VirtualMachines)
	}
}

// The read-only guarantee is a property of the package, so it is asserted
// against the whole command sequence a rich host produces rather than trusted to
// review. A verb that mutates has no business appearing here at all.
func TestScanOnlyEverReadsTheHost(t *testing.T) {
	host := newFakeHost().withRunning(firstUUID).withSleeping(secondUUID)
	host.namespaces = append(host.namespaces, "atlas-deadbeefdead")
	host.directories = append(host.directories, "lost+found")

	if _, err := host.scan(t); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	for _, command := range host.commands.issued {
		if !isReadOnly(command) {
			t.Errorf("scan issued a command that is not a read: %s", command)
		}
	}
}

var readOnlyCommands = []string{
	"ls -1 ",
	"systemctl list-units ",
	"ip netns list",
	"ip -o link show",
	"ip -6 neigh show proxy",
	"sudo lvs ",
	"sudo test -d ",
	"sudo test -S ",
	"sudo cat ",
}

func isReadOnly(command string) bool {
	for _, prefix := range readOnlyCommands {
		if strings.HasPrefix(command, prefix) {
			return true
		}
	}
	// The one command that reads inside a namespace: `sudo ip -n <ns> -o link
	// show <device>`.
	//
	// Deliberately NOT `ip netns exec <ns> ip link show <device>`, which runs
	// whatever follows the namespace as root — a netns isolates networking, not
	// the filesystem, so that form is a root shell wearing a network command's
	// clothes, and no sudoers pattern can constrain it (a wildcard for the
	// namespace name also matches a command). `ip -n` takes the namespace as a
	// flag and can only ever re-execute ip itself.
	fields := strings.Fields(command)
	return len(fields) == 8 &&
		strings.Join(fields[:3], " ") == "sudo ip -n" &&
		strings.Join(fields[4:7], " ") == "-o link show"
}
