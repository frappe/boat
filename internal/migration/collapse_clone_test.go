package migration

import (
	"context"
	"testing"
)

// rootCloneTable is the live dm-clone table for the root disk: field 4 is the dest
// LV (the write target), field 5 the nbd source.
const rootCloneTable = "0 20971520 clone " + cloneMetaDev + " " + vmDiskDev + " /dev/nbd0 32768"

func TestCollapseCloneTransparent(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo dmsetup info "+vmCloneRoot).
		exists("sudo lvs --noheadings atlas/"+cloneMetaRoot).
		output("sudo dmsetup table "+vmCloneRoot, rootCloneTable).
		output("sudo dmsetup status "+vmCloneRoot, "0 20971520 clone 8/1024 32768 640/640 0 rw")

	if err := CollapseClone(context.Background(), fake, testUUID, CollapseCloneParams{}); err != nil {
		t.Fatalf("CollapseClone: %v", err)
	}

	assertTrace(t, fake,
		"? sudo dmsetup info "+vmCloneRoot,
		"sudo dmsetup table "+vmCloneRoot,
		"sudo dmsetup status "+vmCloneRoot,
		// suspend → reload clone->linear onto the dest LV → resume, same major:minor
		"sudo dmsetup suspend "+vmCloneRoot,
		"sudo dmsetup reload "+vmCloneRoot+" --table 0 20971520 linear "+vmDiskDev+" 0",
		"sudo dmsetup resume "+vmCloneRoot,
		// converge: disconnect the source client (unconditional -d, never -check) and
		// drop the clone metadata
		"- sudo nbd-client -d /dev/nbd0",
		"? sudo lvs --noheadings atlas/"+cloneMetaRoot,
		"- sudo lvremove -f atlas/"+cloneMetaRoot,
	)
	// The lying `nbd-client -check` is never consulted.
	assertNotIssued(t, fake, "nbd-client -check")
	// The clone is collapsed transparently, never removed (that would fail busy).
	assertNotIssued(t, fake, "dmsetup remove")
}

// A partially-hydrated clone (an early re-entry) is refused, not collapsed onto holes.
func TestCollapseCloneRefusesPartialHydration(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo dmsetup info "+vmCloneRoot).
		output("sudo dmsetup table "+vmCloneRoot, rootCloneTable).
		output("sudo dmsetup status "+vmCloneRoot, "0 20971520 clone 8/1024 32768 320/640 0 rw")

	if err := CollapseClone(context.Background(), fake, testUUID, CollapseCloneParams{}); err == nil {
		t.Fatal("CollapseClone collapsed a half-hydrated clone")
	}
	assertNotIssued(t, fake, "dmsetup suspend")
	assertNotIssued(t, fake, "dmsetup reload")
}

// An already-linear table (a re-entry after collapse) is a no-op that still converges
// the nbd and metadata teardown.
func TestCollapseCloneAlreadyLinear(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo dmsetup info "+vmCloneRoot).
		output("sudo dmsetup table "+vmCloneRoot, "0 20971520 linear "+vmDiskDev+" 0")

	if err := CollapseClone(context.Background(), fake, testUUID, CollapseCloneParams{}); err != nil {
		t.Fatalf("CollapseClone: %v", err)
	}
	assertNotIssued(t, fake, "dmsetup suspend")
	assertIssued(t, fake, "sudo nbd-client -d /dev/nbd0")
}

// A missing clone device converges the teardown and does nothing else.
func TestCollapseCloneMissingDevice(t *testing.T) {
	fake := newFakeCommands() // no clone present
	if err := CollapseClone(context.Background(), fake, testUUID, CollapseCloneParams{}); err != nil {
		t.Fatalf("CollapseClone: %v", err)
	}
	assertTrace(t, fake,
		"? sudo dmsetup info "+vmCloneRoot,
		"- sudo nbd-client -d /dev/nbd0",
		"? sudo lvs --noheadings atlas/"+cloneMetaRoot,
	)
}

// The data clone collapses onto its OWN dest (atlas-data-<uuid>, read from the live
// table's field 4), not the atlas-vm-<uuid>-data the Python reconstructs and gets
// wrong.
func TestCollapseCloneDataDiskDest(t *testing.T) {
	dataDiskDev := "/dev/atlas/" + dataDisk
	dataCloneTable := "0 41943040 clone /dev/atlas/atlas-clonemeta-3f2504e0-4f89-41d3-9a0c-0305e82c3301-data " + dataDiskDev + " /dev/nbd1 32768"
	fake := newFakeCommands().
		exists("sudo dmsetup info "+vmCloneRoot).
		exists("sudo dmsetup info "+vmCloneData).
		output("sudo dmsetup table "+vmCloneRoot, rootCloneTable).
		output("sudo dmsetup status "+vmCloneRoot, "0 20971520 clone 8/1024 32768 640/640 0 rw").
		output("sudo dmsetup table "+vmCloneData, dataCloneTable).
		output("sudo dmsetup status "+vmCloneData, "0 41943040 clone 9/1024 32768 1280/1280 0 rw")

	if err := CollapseClone(context.Background(), fake, testUUID, CollapseCloneParams{DataDiskGB: 20}); err != nil {
		t.Fatalf("CollapseClone: %v", err)
	}
	// The data clone's linear map lands on atlas-data-<uuid>, and its nbd client is on
	// slot 1 (base+1).
	assertIssued(t, fake, "sudo dmsetup reload "+vmCloneData+" --table 0 41943040 linear "+dataDiskDev+" 0")
	assertIssued(t, fake, "sudo nbd-client -d /dev/nbd1")
	assertNotIssued(t, fake, "linear /dev/atlas/atlas-vm-3f2504e0-4f89-41d3-9a0c-0305e82c3301-data 0")
}
