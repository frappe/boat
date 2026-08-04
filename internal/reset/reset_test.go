// The harness ResetServer runs on: a populated host described as the answers its
// commands give, plus a recorder of every command issued. Nothing here needs nft,
// lvm, dmsetup, nbd or root. The full sweep is asserted as one golden — a reset that
// silently skips a step leaves stranded state a later provision trips over — and the
// parsers that bit Atlas on real hosts get their own unit tests. Mirrors the
// scripts/reset-server.py enumeration.

package reset

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/paths"
)

const testUUID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
const testHex = "3f2504e04f8941d39a0c0305e82c3301"

type fakeCommands struct {
	outputs map[string]string
	trace   []string
}

func newFakeCommands() *fakeCommands { return &fakeCommands{outputs: map[string]string{}} }

func (fake *fakeCommands) output(command, text string) *fakeCommands {
	fake.outputs[command] = text
	return fake
}

// RunUnchecked records every command with the "- " best-effort prefix (reset makes
// no other kind of call) and returns any scripted output.
func (fake *fakeCommands) RunUnchecked(_ context.Context, template string, parameters ...any) (string, error) {
	command := render(template, parameters...)
	fake.trace = append(fake.trace, "- "+command)
	return fake.outputs[command], nil
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

func assertTrace(t *testing.T, fake *fakeCommands, expected ...string) {
	t.Helper()
	if len(fake.trace) != len(expected) {
		t.Fatalf("command sequence:\ngot (%d):\n  %s\nwant (%d):\n  %s",
			len(fake.trace), strings.Join(fake.trace, "\n  "),
			len(expected), strings.Join(expected, "\n  "))
	}
	for index := range expected {
		if fake.trace[index] != expected[index] {
			t.Errorf("command %d:\ngot:  %s\nwant: %s", index, fake.trace[index], expected[index])
		}
	}
}

// A fully populated host: one VM, one forward tunnel, one atlas netns, a veth + a
// tap, one NDP proxy entry, one wake rule + counter, one bound nbd + one free, one
// dm-clone target, and two removable LVs beside the kept pool0. The whole sweep is
// one ordered golden, including the injected per-VM vm-network-down.
func TestResetServerSweepsThePopulatedHost(t *testing.T) {
	vmDir := paths.VirtualMachinesDirectory
	fake := newFakeCommands().
		output("ls -1 "+vmDir, testUUID+"\n").
		output("systemctl list-units atlas-mig6-* --all --no-legend --plain",
			"atlas-mig6-52001.service loaded active running socat carrier\n").
		output("ip netns list", "atlas-3f2504e04f89 (id: 0)\nsome-other-ns\n").
		output("ip -o link show",
			"2: eth0: <BROADCAST> mtu 1500\n5: veth-3f25@if4: <BROADCAST> mtu 1500\n6: tap0: <BROADCAST> mtu 1500\n").
		output("ip -6 neigh show proxy", "2400:6180:100:d0:0:1:5835:d003 dev eth0 proxy\n").
		output("sudo nft -a list chain inet atlas forward",
			"\t\tip6 daddr 2400:6180::1 tcp flags syn counter name wake_"+testHex+" drop # handle 7\n").
		output("sudo nft -j list counters table inet atlas",
			`{"nftables":[{"metainfo":{"version":"1.0.9"}},{"counter":{"family":"inet","name":"wake_`+testHex+`","table":"atlas","packets":0,"bytes":0}}]}`).
		output("ls -1 /sys/block", "nbd0\nnbd1\nsda\n").
		output("cat /sys/block/nbd0/size", "2097152\n").
		output("cat /sys/block/nbd1/size", "0\n").
		output("sudo dmsetup ls --target clone", "atlas-vm-"+testUUID+"-clone\t(253:0)\n").
		output("sudo lvs --noheadings -o lv_name atlas",
			"  pool0\n  atlas-vm-"+testUUID+"\n  atlas-image-debian12\n")

	var networkDownFor []string
	networkDown := func(_ context.Context, uuid string) error {
		networkDownFor = append(networkDownFor, uuid)
		fake.trace = append(fake.trace, "vm-network-down "+uuid)
		return nil
	}

	result, err := ResetServer(context.Background(), fake, ResetParams{}, networkDown)
	if err != nil {
		t.Fatalf("ResetServer: %v", err)
	}

	assertTrace(t, fake,
		// stopVirtualMachines
		"- ls -1 "+vmDir,
		"- sudo systemctl disable --now firecracker-vm@"+testUUID+".service",
		"vm-network-down "+testUUID,
		"- sudo rm -rf "+vmDir+"/"+testUUID,
		"- sudo systemctl reset-failed 'firecracker-vm@*'",
		// stopForwardTunnels
		"- systemctl list-units atlas-mig6-* --all --no-legend --plain",
		"- sudo systemctl stop atlas-mig6-52001.service",
		"- sudo systemctl reset-failed 'atlas-mig6-*'",
		// teardownNetworking: netns, links, ndp, park sweep
		"- ip netns list",
		"- sudo ip netns del atlas-3f2504e04f89",
		"- ip -o link show",
		"- sudo ip link del veth-3f25",
		"- sudo ip link del tap0",
		"- ip -6 neigh show proxy",
		"- sudo ip -6 neigh del proxy 2400:6180:100:d0:0:1:5835:d003 dev eth0",
		"- sudo nft -a list chain inet atlas forward",
		"- sudo nft delete rule inet atlas forward handle 7",
		"- sudo nft -j list counters table inet atlas",
		"- sudo nft delete counter inet atlas wake_"+testHex,
		// disconnectNBDAndDMClone
		"- ls -1 /sys/block",
		"- cat /sys/block/nbd0/size",
		"- cat /sys/block/nbd1/size",
		"- sudo nbd-client -d /dev/nbd0",
		"- sudo dmsetup ls --target clone",
		"- sudo dmsetup remove atlas-vm-"+testUUID+"-clone",
		// removeLogicalVolumes (pool0 skipped)
		"- sudo lvs --noheadings -o lv_name atlas",
		"- sudo lvremove -f atlas/atlas-vm-"+testUUID,
		"- sudo lvremove -f atlas/atlas-image-debian12",
		// clearStateDirectories
		"- sudo find "+vmDir+" -mindepth 1 -maxdepth 1 -exec rm -rf {} +",
		"- sudo find "+paths.ImagesDirectory+" -mindepth 1 -maxdepth 1 -exec rm -rf {} +",
		"- sudo find "+paths.SnapshotsDirectory+" -mindepth 1 -maxdepth 1 -exec rm -rf {} +",
	)

	// The result reports what was swept.
	if len(networkDownFor) != 1 || networkDownFor[0] != testUUID {
		t.Errorf("networkDown called for %v, want [%s]", networkDownFor, testUUID)
	}
	if len(result.VirtualMachines) != 1 || result.VirtualMachines[0] != testUUID {
		t.Errorf("result.VirtualMachines = %v", result.VirtualMachines)
	}
	if len(result.LogicalVolumes) != 2 {
		t.Errorf("result.LogicalVolumes = %v, want the two non-pool LVs", result.LogicalVolumes)
	}
	if len(result.BoundNBD) != 1 || result.BoundNBD[0] != "/dev/nbd0" {
		t.Errorf("result.BoundNBD = %v, want [/dev/nbd0] (nbd1 is free)", result.BoundNBD)
	}
}

// A just-bootstrapped host (nothing to sweep) is a clean, side-effect-free run: the
// enumerations return empty, only the pool0 LV is seen (and skipped), and the three
// directory sweeps still run.
func TestResetServerOnAnEmptyHostIsANoOp(t *testing.T) {
	fake := newFakeCommands().
		output("sudo lvs --noheadings -o lv_name atlas", "  pool0\n")

	result, err := ResetServer(context.Background(), fake, ResetParams{}, nil)
	if err != nil {
		t.Fatalf("ResetServer: %v", err)
	}
	for _, command := range fake.trace {
		if strings.Contains(command, "lvremove") || strings.Contains(command, "disable --now") ||
			strings.Contains(command, "netns del") || strings.Contains(command, "nbd-client") {
			t.Errorf("a mutation ran on an empty host: %q", command)
		}
	}
	if result.VirtualMachines != nil || result.LogicalVolumes != nil {
		t.Errorf("empty host reported swept state: %+v", result)
	}
}

// A non-UUID directory entry never becomes a systemctl target or an rm path — it is
// skipped by the UUID guard and left for the final directory sweep.
func TestListVMDirectoriesRejectsNonUUID(t *testing.T) {
	fake := newFakeCommands().
		output("ls -1 "+paths.VirtualMachinesDirectory, testUUID+"\n..\nnot-a-uuid\n")
	got := listVMDirectories(context.Background(), fake)
	if len(got) != 1 || got[0] != testUUID {
		t.Errorf("listVMDirectories = %v, want only the real UUID", got)
	}
}

func TestListUnitsKeepsOnlyServiceRows(t *testing.T) {
	fake := newFakeCommands().output("systemctl list-units p --all --no-legend --plain",
		"atlas-mig6-1.service loaded active running x\natlas-mig6-1.timer loaded active waiting y\n\n")
	got := listUnits(context.Background(), fake, "p")
	if len(got) != 1 || got[0] != "atlas-mig6-1.service" {
		t.Errorf("listUnits = %v, want only the .service row", got)
	}
}

func TestListAtlasLinksSplitsOnAtAndFiltersByPrefix(t *testing.T) {
	fake := newFakeCommands().output("ip -o link show",
		"1: lo: <LOOPBACK>\n2: eth0: <BROADCAST>\n5: veth-abc@if9: <BROADCAST>\n6: tap0: <BROADCAST>\n7: mig6-52001: <POINTOPOINT>\n")
	got := listAtlasLinks(context.Background(), fake)
	want := []string{"veth-abc", "tap0", "mig6-52001"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("listAtlasLinks = %v, want %v", got, want)
	}
}

func TestListNDPProxyReadsAddressAndDevice(t *testing.T) {
	fake := newFakeCommands().output("ip -6 neigh show proxy",
		"2400:6180::1 dev eth0 proxy\n2400:6180::2 dev ens3 proxy\n")
	got := listNDPProxy(context.Background(), fake)
	if len(got) != 2 || got[0] != (ndpEntry{"2400:6180::1", "eth0"}) || got[1] != (ndpEntry{"2400:6180::2", "ens3"}) {
		t.Errorf("listNDPProxy = %+v", got)
	}
}

func TestListWakeCountersSkipsMetainfoAndNonWake(t *testing.T) {
	fake := newFakeCommands().output("sudo nft -j list counters table inet atlas",
		`{"nftables":[{"metainfo":{"version":"1"}},{"counter":{"name":"wake_`+testHex+`"}},{"counter":{"name":"other"}}]}`)
	got := listWakeCounters(context.Background(), fake)
	if len(got) != 1 || got[0] != "wake_"+testHex {
		t.Errorf("listWakeCounters = %v, want only the wake_ counter", got)
	}
	// Unparseable output (no table) yields nothing rather than panicking.
	empty := newFakeCommands().output("sudo nft -j list counters table inet atlas", "Error: No such file or directory\n")
	if got := listWakeCounters(context.Background(), empty); got != nil {
		t.Errorf("listWakeCounters on junk = %v, want nil", got)
	}
}

func TestListDMTargetsHandlesNoDevices(t *testing.T) {
	present := newFakeCommands().output("sudo dmsetup ls --target clone", "atlas-vm-x-clone\t(253:0)\n")
	if got := listDMTargets(context.Background(), present); len(got) != 1 || got[0] != "atlas-vm-x-clone" {
		t.Errorf("listDMTargets = %v", got)
	}
	empty := newFakeCommands().output("sudo dmsetup ls --target clone", "No devices found\n")
	if got := listDMTargets(context.Background(), empty); got != nil {
		t.Errorf("listDMTargets on empty = %v, want nil", got)
	}
}
