package vmdisk

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/paths"
)

const testUUID = "dead0000-0000-4000-8000-000000000001"

type fakeCommands struct {
	outputs map[string]string
	present map[string]bool
	trace   []string
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{outputs: map[string]string{}, present: map[string]bool{}}
}

func (fake *fakeCommands) output(command, text string) *fakeCommands {
	fake.outputs[command] = text
	return fake
}
func (fake *fakeCommands) exists(command string) *fakeCommands {
	fake.present[command] = true
	return fake
}

func (fake *fakeCommands) Run(_ context.Context, template string, parameters ...any) (string, error) {
	command := render(template, parameters...)
	fake.trace = append(fake.trace, command)
	return fake.outputs[command], nil
}

func (fake *fakeCommands) OK(_ context.Context, template string, parameters ...any) bool {
	command := render(template, parameters...)
	fake.trace = append(fake.trace, "? "+command)
	return fake.present[command]
}

func render(template string, parameters ...any) string {
	parts := strings.Split(template, "{}")
	if len(parts)-1 != len(parameters) {
		panic(fmt.Sprintf("%q: %d placeholders, %d parameters", template, len(parts)-1, len(parameters)))
	}
	var builder strings.Builder
	for index, part := range parts {
		builder.WriteString(part)
		if index < len(parameters) {
			fmt.Fprintf(&builder, "%v", parameters[index])
		}
	}
	return builder.String()
}

func assertTrace(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("command count %d, want %d:\ngot:\n  %s\nwant:\n  %s",
			len(got), len(want), strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("command %d:\ngot:  %s\nwant: %s", index, got[index], want[index])
		}
	}
}

func newTestBringUp(fake *fakeCommands) *bringUp { return &bringUp{commands: fake, uuid: testUUID} }

// A VM with a root disk and no data disk: activate the LV (a snapshot, so -K), wait
// for udev, and re-mknod the jail node with the LV's major:minor. The data disk is
// probed and, absent, skipped.
func TestBringUpRootDiskActivatesAndExposes(t *testing.T) {
	rootNode := paths.ForVirtualMachine(testUUID).RootFilesystemNode()
	rootDevice := "/dev/atlas/atlas-vm-" + testUUID
	fake := newFakeCommands().
		exists("test -b "+rootDevice).
		output("lsblk -ndo MAJ:MIN "+rootDevice, "252:5  ")

	bringUp := newTestBringUp(fake)
	if err := bringUp.bringDiskUp(context.Background(), "", "atlas-vm-"+testUUID, rootNode, 255999, true); err != nil {
		t.Fatalf("bringDiskUp: %v", err)
	}
	assertTrace(t, fake.trace, []string{
		"? sudo dmsetup info atlas-vm-" + testUUID + "-clone",
		"sudo lvchange -ay -K atlas/atlas-vm-" + testUUID,
		"sudo udevadm settle",
		"? test -b " + rootDevice,
		"lsblk -ndo MAJ:MIN " + rootDevice,
		"sudo rm -f " + rootNode,
		"sudo mknod " + rootNode + " b 252 5",
		"sudo chown 255999:255999 " + rootNode,
		"sudo chmod 0660 " + rootNode,
	})
}

// A VM with no data disk: the data LV is probed and, absent, the disk is skipped
// (not an error) — no activate, no mknod.
func TestBringUpDataDiskIsSkippedWhenAbsent(t *testing.T) {
	dataNode := paths.ForVirtualMachine(testUUID).DataNode()
	fake := newFakeCommands() // data clone absent, data LV absent

	if err := newTestBringUp(fake).bringDiskUp(context.Background(), "-data", "atlas-data-"+testUUID, dataNode, 255999, false); err != nil {
		t.Fatalf("bringDiskUp: %v", err)
	}
	assertTrace(t, fake.trace, []string{
		"? sudo dmsetup info atlas-vm-" + testUUID + "-data-clone",
		"? sudo lvs --noheadings atlas/atlas-data-" + testUUID,
	})
	for _, command := range fake.trace {
		if strings.Contains(command, "lvchange") || strings.Contains(command, "mknod") {
			t.Errorf("an absent data disk was activated or exposed: %q", command)
		}
	}
}

// A migration boot-then-hydrate: a dm-clone exists, so the guest reads THROUGH it
// — the clone device is exposed, and the plain LV is never activated (it is the
// incomplete hydration destination).
func TestBringUpExposesTheCloneWhenMigrating(t *testing.T) {
	rootNode := paths.ForVirtualMachine(testUUID).RootFilesystemNode()
	cloneName := "atlas-vm-" + testUUID + "-clone"
	cloneDevice := "/dev/mapper/" + cloneName
	fake := newFakeCommands().
		exists("sudo dmsetup info "+cloneName).
		output("lsblk -ndo MAJ:MIN "+cloneDevice, "253:0")

	if err := newTestBringUp(fake).bringDiskUp(context.Background(), "", "atlas-vm-"+testUUID, rootNode, 255999, true); err != nil {
		t.Fatalf("bringDiskUp: %v", err)
	}
	if fake.contains("lvchange") {
		t.Error("the plain LV was activated while a clone was live")
	}
	if !fake.contains("mknod " + rootNode + " b 253 0") {
		t.Error("the clone device was not exposed in the jail")
	}
}

// The activate falls back to vgmknodes when udev has not made the node yet.
func TestActivateFallsBackToVgmknodes(t *testing.T) {
	device := "/dev/atlas/atlas-vm-" + testUUID
	fake := newFakeCommands() // test -b false the first time, false again → error path...
	// Make it present only after vgmknodes: the fake's OK is stateless, so present
	// the node from the start for the success case and assert the fallback shape via
	// a separate run where it never appears.
	fake.exists("test -b " + device) // present → no fallback
	if err := newTestBringUp(fake).activate(context.Background(), "atlas/atlas-vm-"+testUUID, device); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if fake.contains("vgmknodes") {
		t.Error("vgmknodes ran even though the node was present after settle")
	}

	// Now a host where the node never appears: activate must run vgmknodes and then
	// fail loud.
	missing := newFakeCommands()
	if err := newTestBringUp(missing).activate(context.Background(), "atlas/atlas-vm-"+testUUID, device); err == nil {
		t.Fatal("activate did not fail when the node never became a block device")
	}
	if !missing.contains("sudo vgmknodes atlas") {
		t.Error("activate did not fall back to vgmknodes")
	}
}

func TestParseDeviceNumberStripsPadding(t *testing.T) {
	major, minor, err := parseDeviceNumber("252:5  \n")
	if err != nil || major != 252 || minor != 5 {
		t.Errorf("parseDeviceNumber = %d:%d, %v; want 252:5", major, minor, err)
	}
	if _, _, err := parseDeviceNumber("not-a-number"); err == nil {
		t.Error("parseDeviceNumber accepted junk")
	}
}

func TestValidUUID(t *testing.T) {
	if !validUUID(testUUID) {
		t.Error("rejected a valid UUID")
	}
	for _, bad := range []string{"", "dead", "DEAD0000-0000-4000-8000-000000000001", "dead0000-0000-4000-8000-00000000000", "dead0000;0000-4000-8000-000000000001"} {
		if validUUID(bad) {
			t.Errorf("accepted a bad UUID %q", bad)
		}
	}
}

func (fake *fakeCommands) contains(fragment string) bool {
	for _, command := range fake.trace {
		if strings.Contains(command, fragment) {
			return true
		}
	}
	return false
}
