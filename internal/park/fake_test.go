// The harness every scenario in this package runs on: a host described as the
// answers its commands give, plus the two seams park and the trap have.
//
// Nothing here needs nft, root, a network or a VM. The commands are spelled out
// as literal strings rather than derived, so a template that drifts from the one
// scripts/lib/atlas/park.py renders shows up as a failing golden rather than as
// a host that quietly stops trapping SYNs.

package park

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/frappe/boat/internal/run"
	"log/slog"
	"strings"
	"testing"
	"time"
)

var errCommandFailed = errors.New("command failed")

const (
	// The UUID and address scripts/lib/atlas/test_park.py uses, so the two test
	// suites can be compared line by line.
	testUUID    = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	testHex     = "3f2504e04f8941d39a0c0305e82c3301"
	testAddress = "2400:6180:100:d0:0:1:5835:d003"

	otherUUID    = "11111111-2222-3333-4444-555555555555"
	otherHex     = "11111111222233334444555555555555"
	otherAddress = "2001:db8::2"
)

// Every command this package renders, written out the way an operator sees it in
// a journal. A rule that drifts silently is a rule that stops trapping, so these
// are text and not a second copy of the builders.
const (
	deviceShow = "ip link show atlas-park0"
	deviceAdd  = "sudo ip link add atlas-park0 type dummy"
	deviceUp   = "sudo ip link set atlas-park0 up"

	listTable = "sudo nft list table inet atlas"
	addTable  = "sudo nft add table inet atlas"
	listChain = "sudo nft list chain inet atlas forward"
	addChain  = "sudo nft add chain inet atlas forward " +
		"{ type filter hook forward priority filter; policy accept; }"

	defaultRoute = "ip -j -6 route show default"
	neighReplace = "sudo ip -6 neigh replace proxy " + testAddress + " dev eth0"
	routeReplace = "sudo ip -6 route replace " + testAddress + "/128 dev atlas-park0"
	routeDelete  = "sudo ip -6 route del " + testAddress + "/128 dev atlas-park0"

	listCounter   = "sudo nft list counter inet atlas wake_" + testHex
	addCounter    = "sudo nft add counter inet atlas wake_" + testHex
	deleteCounter = "sudo nft delete counter inet atlas wake_" + testHex
	listHandles   = "sudo nft -a list chain inet atlas forward"
	listCounters  = "sudo nft -j list counters table inet atlas"

	// The trap itself. Every clause of this line is load-bearing; see rules.go.
	addRule = "sudo nft add rule inet atlas forward ip6 daddr " + testAddress +
		" tcp flags syn / fin,syn,rst,ack counter name wake_" + testHex + " drop"

	listVirtualMachines = "sudo ls -1 /var/lib/atlas/virtual-machines"
)

// A host with an IPv6 uplink answers this, and `ip -j` is JSON because scraping
// the plain form is how a device name silently becomes "via".
const defaultRouteOutput = `[{"dst":"default","gateway":"fe80::1","dev":"eth0","metric":1024,"flags":[]}]`

func markerOf(uuid string) string {
	return "sudo test -f /var/lib/atlas/virtual-machines/" + uuid + "/sleeping"
}

// sidecarProbe is the presence question asked before the sidecar is read, so a
// test can script "there" and "could not look" separately.
func sidecarProbe(uuid string) string {
	return "sudo test -f " + testFiles(uuid).networkEnvironment
}

func environmentOf(uuid string) string {
	return "sudo cat /var/lib/atlas/virtual-machines/" + uuid + "/network.env"
}

func environmentText(address string) string {
	return "# written by provision-vm.py\nTAP_DEVICE=atlas-3f2504e04\n" +
		"VIRTUAL_MACHINE_IPV6=" + address + "\nATLAS_NETNS=atlas-3f2504e04f89\nATLAS_FC_UID=247312\n"
}

// testFiles spells out the slice of the path layout this package addresses a VM
// through, rather than deriving it, so the golden command lines read like the
// ones on a host.
func testFiles(uuid string) virtualMachineFiles {
	directory := "/var/lib/atlas/virtual-machines/" + uuid
	return virtualMachineFiles{
		sleepingMarker:     directory + "/sleeping",
		networkEnvironment: directory + "/network.env",
	}
}

// fakeCommands answers rendered commands from a script and records every one of
// them.
//
// A recorded line carries a prefix for how much the command's failure mattered:
// "? " for a boolean gate, "- " for a discarded exit code, nothing for a command
// whose failure aborts the verb. The sequence therefore shows not only what ran
// but which parts were best-effort, which is most of what a port of this gets
// wrong.
type fakeCommands struct {
	outputs     map[string]string
	present     map[string]bool
	failing     map[string]bool
	directories []string
	trace       []string
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{
		outputs: map[string]string{},
		present: map[string]bool{},
		failing: map[string]bool{},
	}
}

// withScaffold is the ordinary host: the bootstrap dummy is up, the nft table
// and forward chain the network bring-up creates are there, and there is an
// IPv6 default route to answer NDP on.
func (fake *fakeCommands) withScaffold() *fakeCommands {
	fake.present[deviceShow] = true
	fake.present[listTable] = true
	fake.present[listChain] = true
	fake.outputs[defaultRoute] = defaultRouteOutput
	return fake
}

// withSleeping describes a VM this host is holding asleep: a directory in the
// listing, a marker on disk, and a sidecar naming its /128. That trio is the
// whole of what the boot sweep reads — no database is consulted anywhere.
func (fake *fakeCommands) withSleeping(uuid string, address string) *fakeCommands {
	fake.withDirectory(uuid)
	fake.present[markerOf(uuid)] = true
	fake.present[sidecarProbe(uuid)] = true
	fake.outputs[environmentOf(uuid)] = environmentText(address)
	return fake
}

// withDirectory adds a VM directory with no marker: a VM that is not asleep.
func (fake *fakeCommands) withDirectory(uuid string) *fakeCommands {
	fake.directories = append(fake.directories, uuid)
	fake.outputs[listVirtualMachines] = strings.Join(fake.directories, "\n") + "\n"
	return fake
}

func (fake *fakeCommands) exists(command string) *fakeCommands {
	fake.present[command] = true
	return fake
}

func (fake *fakeCommands) fails(command string) *fakeCommands {
	fake.failing[command] = true
	return fake
}

func (fake *fakeCommands) output(command string, text string) *fakeCommands {
	fake.outputs[command] = text
	return fake
}

func (fake *fakeCommands) record(prefix string, command string) {
	fake.trace = append(fake.trace, prefix+command)
}

func (fake *fakeCommands) Run(_ context.Context, template string, parameters ...any) (string, error) {
	command := render(template, parameters...)
	fake.record("", command)
	if fake.failing[command] {
		return "", errCommandFailed
	}
	return fake.outputs[command], nil
}

func (fake *fakeCommands) RunUnchecked(
	_ context.Context, template string, parameters ...any,
) (string, error) {
	command := render(template, parameters...)
	fake.record("- ", command)
	if fake.failing[command] {
		return "", errCommandFailed
	}
	return fake.outputs[command], nil
}

// OK defaults to false: an artifact exists in a scenario only because the
// scenario said so, so a probe nobody scripted reads as absent.
func (fake *fakeCommands) OK(_ context.Context, template string, parameters ...any) bool {
	command := render(template, parameters...)
	fake.record("? ", command)
	return fake.present[command]
}

// Probe answers in three values, so a test can say "denied" as well as "no".
// The bool the old gate returned could not, which is how a denied `cat` became
// "this VM has no address" became "no trap needed" became a green sleep.
func (fake *fakeCommands) Probe(
	_ context.Context, template string, parameters ...any,
) (run.Answer, error) {
	command := render(template, parameters...)
	fake.record("? ", command)
	if fake.failing[command] {
		return run.Unknown, fmt.Errorf("could not run %s", command)
	}
	if fake.present[command] {
		return run.Yes, nil
	}
	return run.No, nil
}

func (fake *fakeCommands) issued(fragment string) bool {
	for _, recorded := range fake.trace {
		if strings.Contains(recorded, fragment) {
			return true
		}
	}
	return false
}

func (fake *fakeCommands) indexOf(t *testing.T, fragment string) int {
	t.Helper()
	for index, recorded := range fake.trace {
		if strings.Contains(recorded, fragment) {
			return index
		}
	}
	t.Fatalf("no command matching %q was issued:\n  %s", fragment, strings.Join(fake.trace, "\n  "))
	return -1
}

// render substitutes each {} with its parameter the way run.Render does, minus
// the shell quoting — every value here is a path, an address or a device name,
// and an unquoted line is the one a reader can compare to the Python by eye. It
// panics on an arity mismatch, which catches a miscounted template for free.
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

func newTestParker(fake *fakeCommands) *parker {
	return &parker{commands: fake, filesFor: testFiles}
}

// fakeClock ends the loop after a fixed number of waits, so a one-second poll
// costs a test nothing while the loop under test stays the real one.
type fakeClock struct {
	ticks  int
	waited []time.Duration
}

func (clock *fakeClock) Wait(_ context.Context, duration time.Duration) bool {
	clock.waited = append(clock.waited, duration)
	if clock.ticks <= 0 {
		return false
	}
	clock.ticks--
	return true
}

func newTestTrap(fake *fakeCommands, wake func(ctx context.Context, uuid string) error) *Trap {
	return &Trap{
		poller:  newTestParker(fake),
		sweeper: newTestParker(fake),
		wake:    wake,
		clock:   &fakeClock{},
	}
}

// wakeCounter is one named counter as nft reports it.
type wakeCounter struct {
	name    string
	packets int64
}

// counterListing renders `nft -j list counters table inet atlas`. The metainfo
// element is included because a real host always sends one and a parser that
// chokes on it reads every host as having no sleeping VMs.
func counterListing(counters ...wakeCounter) string {
	elements := []string{`{"metainfo":{"version":"1.0.9","json_schema_version":1}}`}
	for _, counter := range counters {
		elements = append(elements, fmt.Sprintf(
			`{"counter":{"family":"inet","name":%q,"table":"atlas","handle":3,"packets":%d,"bytes":%d}}`,
			counter.name, counter.packets, counter.packets*60,
		))
	}
	return `{"nftables":[` + strings.Join(elements, ",") + `]}`
}

// captureJournal swaps the default logger for one writing into a buffer, so a
// test can assert what the trap did — and did not — say.
func captureJournal(t *testing.T) *bytes.Buffer {
	t.Helper()
	journal := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(journal, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return journal
}

func assertTrace(t *testing.T, fake *fakeCommands, expected ...string) {
	t.Helper()
	if len(fake.trace) != len(expected) {
		t.Fatalf("command sequence:\ngot:\n  %s\nwant:\n  %s",
			strings.Join(fake.trace, "\n  "), strings.Join(expected, "\n  "))
	}
	for index := range expected {
		if fake.trace[index] != expected[index] {
			t.Errorf("command %d:\ngot:  %s\nwant: %s", index, fake.trace[index], expected[index])
		}
	}
}

func assertNotIssued(t *testing.T, fake *fakeCommands, fragment string) {
	t.Helper()
	if fake.issued(fragment) {
		t.Errorf("issued %q, want it not to:\n  %s", fragment, strings.Join(fake.trace, "\n  "))
	}
}
