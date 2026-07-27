package adopt

import (
	"slices"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/model"
)

// The parsers are fed the output shapes the real commands produce, because that
// is the only part of this package a host could disagree with. Each case is a
// line these commands actually print, not a line convenient to parse.

func TestParseUnitsReadsTheLivenessColumns(t *testing.T) {
	output := "firecracker-vm@" + firstUUID + ".service loaded active running Firecracker VM " + firstUUID + "\n" +
		"firecracker-vm@" + secondUUID + ".service loaded failed failed Firecracker VM\n" +
		"\n"

	units := parseUnits(output)

	if len(units) != 2 {
		t.Fatalf("units = %+v, want 2", units)
	}
	if units[0] != (model.UnitLiveness{
		Name: "firecracker-vm@" + firstUUID + ".service", ActiveState: "active", SubState: "running",
	}) {
		t.Errorf("unit = %+v, want the name, ActiveState and SubState", units[0])
	}
	if units[1].ActiveState != "failed" || units[1].SubState != "failed" {
		t.Errorf("unit = %+v, want failed/failed reported as the host said it", units[1])
	}
}

// A not-found instance is a name systemd holds for a unit file that does not
// exist. Reading it as a VM invents a UUID no artifact on the host carries.
func TestParseUnitsSkipsNotFoundInstances(t *testing.T) {
	output := "firecracker-vm@" + firstUUID + ".service not-found inactive dead firecracker-vm@\n"

	if units := parseUnits(output); len(units) != 0 {
		t.Errorf("units = %+v, want none", units)
	}
}

func TestParseNamespacesTakesTheNameOffEachLine(t *testing.T) {
	namespaces := parseNamespaces("atlas-111122223333 (id: 0)\natlas-aaaabbbbcccc\n\n")

	if !slices.Equal(namespaces, []string{"atlas-111122223333", "atlas-aaaabbbbcccc"}) {
		t.Errorf("namespaces = %v", namespaces)
	}
}

func TestParseLinksTakesTheNameBeforeThePeerSuffix(t *testing.T) {
	output := "1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN\\    link/loopback\n" +
		"5: atlas-h1111222@if4: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500\\    link/ether\n" +
		"garbage without a colon space\n"

	links := parseLinks(output)

	if !slices.Equal(links, []string{"lo", "atlas-h1111222"}) {
		t.Errorf("links = %v", links)
	}
}

func TestParseProxiesReadsAddressAndDevice(t *testing.T) {
	output := "2001:db8::1 dev eth0 proxy\nmalformed line\n"

	proxies := parseProxies(output)

	if len(proxies) != 1 || proxies[0].address != "2001:db8::1" || proxies[0].device != "eth0" {
		t.Errorf("proxies = %+v", proxies)
	}
}

// pool_lv and origin are empty for a plain LV, which is why the separator is
// explicit: in the default whitespace-aligned output an empty column and a
// missing one look the same.
func TestParseVolumesReadsEmptyColumns(t *testing.T) {
	output := "  pool0,107374182400,,\n" +
		"  atlas-vm-" + firstUUID + ",10737418240,pool0,atlas-image-bench\n" +
		"  broken,notanumber,pool0,\n" +
		"\n  ,,,\n"

	volumes := parseVolumes(output)

	if len(volumes) != 3 {
		t.Fatalf("volumes = %+v, want 3", volumes)
	}
	if volumes[0] != (model.LogicalVolume{Name: "pool0", SizeBytes: 107374182400}) {
		t.Errorf("volume = %+v, want the pool with no origin and no pool of its own", volumes[0])
	}
	if volumes[1].Pool != "pool0" || volumes[1].Origin != "atlas-image-bench" {
		t.Errorf("volume = %+v, want the thin snapshot's pool and origin", volumes[1])
	}
	// An unreadable size does not cost the host its volume: the coherence rules
	// only ever ask whether a volume exists.
	if volumes[2].Name != "broken" || volumes[2].SizeBytes != 0 {
		t.Errorf("volume = %+v, want the volume reported with a zero size", volumes[2])
	}
}

func TestParseNetworkEnvironmentReadsTheSidecar(t *testing.T) {
	environment := parseNetworkEnvironment(environmentText(firstUUID))

	want := networkingOf[firstUUID]
	if environment.namespace != want.namespace || environment.tap != want.tap ||
		environment.hostVeth != want.hostVeth || environment.address != want.address {
		t.Errorf("environment = %+v, want %+v", environment, want)
	}
	if !environment.complete() {
		t.Error("complete() = false for a fully written sidecar")
	}
}

func TestParseNetworkEnvironmentToleratesQuotesAndComments(t *testing.T) {
	text := "# comment\n\nTAP_DEVICE=\"atlas-x\"\nATLAS_NETNS='atlas-y'\nHOST_VETH=atlas-h\n" +
		"VIRTUAL_MACHINE_IPV6=2001:db8::1\nNOT_A_PAIR\n"

	environment := parseNetworkEnvironment(text)

	if environment.tap != "atlas-x" || environment.namespace != "atlas-y" {
		t.Errorf("environment = %+v, want the quotes stripped", environment)
	}
}

// A half-written sidecar is not a usable one: the teardown hook reads the same
// keys, so a VM missing any of them cannot be taken down cleanly either.
func TestAPartialNetworkEnvironmentIsNotComplete(t *testing.T) {
	environment := parseNetworkEnvironment("TAP_DEVICE=atlas-x\nATLAS_NETNS=atlas-y\n")

	if environment.complete() {
		t.Errorf("complete() = true for %+v", environment)
	}
}

// A UUID comes only from the classes that carry a whole one, and the same UUID
// named by three of them is still one candidate.
func TestCandidatesUnionTheClassesThatCarryAWholeUUID(t *testing.T) {
	taken := inventory{
		directories: []string{secondUUID, "lost+found"},
		units:       []model.UnitLiveness{{Name: "firecracker-vm@" + secondUUID + ".service"}},
		volumes: []model.LogicalVolume{
			{Name: "atlas-vm-" + secondUUID},
			{Name: "atlas-data-" + firstUUID},
			{Name: "atlas-snap-" + firstUUID},
			{Name: "pool0"},
		},
	}

	if candidates := taken.candidates(); !slices.Equal(candidates, []string{firstUUID, secondUUID}) {
		t.Errorf("candidates = %v, want the two UUIDs, sorted", candidates)
	}
}

func TestIsUUID(t *testing.T) {
	cases := map[string]bool{
		firstUUID:                              true,
		"AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE": true,
		"":                                     false,
		"lost+found":                           false,
		"pool0":                                false,
		"11111111-2222-3333-4444-5555555555":   false,
		"gggggggg-2222-3333-4444-555555555555": false,
		"11111111-2222-3333-555555555555":      false,
	}
	for name, want := range cases {
		if isUUID(name) != want {
			t.Errorf("isUUID(%q) = %v, want %v", name, isUUID(name), want)
		}
	}
}

// The enumerations must render exactly as reset-server.py runs them: a template
// that drifts reads back as a host that suddenly holds nothing.
func TestTakeInventoryRunsTheSixEnumerations(t *testing.T) {
	fake := &fakeCommands{
		outputs: map[string]string{}, present: map[string]bool{}, failing: map[string]bool{},
	}

	if _, err := takeInventory(t.Context(), fake); err != nil {
		t.Fatalf("takeInventory: %v", err)
	}
	expected := []string{
		listDirectories, listUnits, listNamespaces, listLinks, listProxies, listVolumes,
	}
	if !slices.Equal(fake.issued, expected) {
		t.Errorf("enumerations:\ngot:\n  %s\nwant:\n  %s",
			strings.Join(fake.issued, "\n  "), strings.Join(expected, "\n  "))
	}
}

// Once one enumeration has failed the rest are skipped: the inventory is
// discarded whole, so asking the host more questions buys nothing.
func TestTakeInventoryStopsAtTheFirstFailure(t *testing.T) {
	fake := &fakeCommands{
		outputs: map[string]string{}, present: map[string]bool{},
		failing: map[string]bool{listUnits: true},
	}

	if _, err := takeInventory(t.Context(), fake); err == nil {
		t.Fatal("takeInventory succeeded, want the failure reported")
	}
	if !slices.Equal(fake.issued, []string{listDirectories, listUnits}) {
		t.Errorf("enumerations = %v, want it to stop at the failure", fake.issued)
	}
}
