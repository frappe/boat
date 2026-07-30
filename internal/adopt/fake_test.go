// The harness every scenario in this package runs on: a host described as the
// lines its commands print, and fakes for the two seams a scan has.

package adopt

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/frappe/boat/internal/fcattach"
	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/run"
)

// Every test in this package drives a whole scenario through canned command
// output, because a scan IS its command sequence plus the reading of what came
// back. Nothing needs a host, which is the point: the enumerations are spelled
// out below as literal strings rather than derived, so a template that drifts
// from what scripts/reset-server.py runs shows up as a host that suddenly looks
// empty.

var errCommandFailed = errors.New("command failed")

const (
	firstUUID  = "11111111-2222-3333-4444-555555555555"
	secondUUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

// The six enumerations, as they render.
const (
	listDirectories = "sudo ls -1 /var/lib/atlas/virtual-machines"
	// The two ways an optional enumeration proves its subject is there before it
	// is read. They are the difference between "not bootstrapped" and "could not
	// read", which is the distinction the scan got wrong.
	//
	// One is a probe and one is a listing, and which is which is not a style
	// choice: `test -d` is silent when its answer is no, so a complaint on stderr
	// can only be a denial; `vgs atlas` explains its no on stderr, so nothing
	// tells its ordinary bare-host answer apart from a denied sudo. Only the
	// first kind can be asked as a question.
	probeDirectory   = "sudo test -d /var/lib/atlas/virtual-machines"
	listVolumeGroups = "sudo vgs --noheadings -o vg_name"
	listUnits        = "systemctl list-units firecracker-vm@* --all --no-legend --plain"
	listNamespaces   = "ip netns list"
	listLinks        = "ip -o link show"
	listProxies      = "ip -6 neigh show proxy"
	listVolumes      = "sudo lvs --noheadings --nosuffix --units b --separator , " +
		"-o lv_name,lv_size,pool_lv,origin atlas"
)

// linksIn is the per-VM namespace listing, and it is a listing for the same
// reason the volume groups are: `ip link show <missing device>` complains on
// stderr and exits 1, exactly as a denied sudo does.
func linksIn(namespace string) string { return "sudo ip -n " + namespace + " -o link show" }

// The on-host layout, spelled out the way an operator sees it in a Task log.
func directoryOf(uuid string) string { return "/var/lib/atlas/virtual-machines/" + uuid }

func jailRootOf(uuid string) string {
	return directoryOf(uuid) + "/jail/firecracker/" + uuid + "/root"
}

func apiSocketOf(uuid string) string { return jailRootOf(uuid) + "/run/firecracker.socket" }

func environmentPathOf(uuid string) string { return directoryOf(uuid) + "/network.env" }

// networking is the artifact names one VM's network.env hands out. They are
// derived from the UUID by Atlas, but this package reads them rather than
// deriving them, so the tests state them as data.
type networking struct{ namespace, tap, hostVeth, address string }

var networkingOf = map[string]networking{
	firstUUID: {
		namespace: "atlas-111122223333", tap: "atlas-111122223",
		hostVeth: "atlas-h1111222", address: "2001:db8::1",
	},
	secondUUID: {
		namespace: "atlas-aaaabbbbcccc", tap: "atlas-aaaabbbbc",
		hostVeth: "atlas-haaaabbb", address: "2001:db8::2",
	},
}

func environmentText(uuid string) string {
	network := networkingOf[uuid]
	return fmt.Sprintf(
		"# written by provision-vm.py\nTAP_DEVICE=%s\nVIRTUAL_MACHINE_IPV6=%s\n"+
			"ATLAS_NETNS=%s\nHOST_VETH=%s\nNAMESPACE_VETH=atlas-n1111222\n"+
			"IPV4_HOST_CIDR=100.64.0.9/30\nATLAS_FC_UID=247312\n",
		network.tap, network.address, network.namespace, network.hostVeth,
	)
}

// fakeHost is one scenario's host: the lines each enumeration returns, plus the
// probes that answer true. Scenarios build a healthy host and then take one
// thing away, which is how every real half-state on a host arises.
type fakeHost struct {
	directories []string
	units       []string
	namespaces  []string
	links       []string
	proxies     []string
	volumes     []string
	present     map[string]bool
	// denied is a probe the host would not let us make: the state every already
	// bootstrapped host is in until its sudoers file is re-installed. It is a
	// THIRD state and not the absence of `present`, because a fake that answered
	// a denied probe the way it answers an absent artifact is a fake that cannot
	// fail the way this package kept failing.
	denied  map[string]bool
	outputs map[string]string
	failing map[string]bool
	// unstartable is a command that cannot be run at all — a missing binary —
	// as opposed to one that runs and exits non-zero.
	unstartable map[string]bool
	statuses    map[string]model.VirtualMachineStatus
	observeFail map[string]bool
	// firecracker is the UUIDs a live Firecracker ANSWERS for, and livenessFail
	// the ones whose probe cannot be made at all. The two are separate because
	// they are separate answers: nothing answering is data about the host, and a
	// probe that failed is data about us.
	firecracker  map[string]bool
	livenessFail map[string]bool

	commands *fakeCommands
	observer *fakeObserver
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		// Every host carries the pool and the park dummy: they are bootstrap
		// floor, and a scan that reported them would report them on every host.
		volumes: []string{"  pool0,107374182400,,"},
		links:   []string{"1: lo: <LOOPBACK,UP> mtu 65536", "2: eth0: <BROADCAST,UP> mtu 1500"},
		// A bootstrapped host by default: both optional subjects are there, so
		// the scan asks for them. The bare-host scenario clears these, which is
		// the only way an optional enumeration is skipped now — a FAILED read is
		// a failed scan, not an empty answer.
		present:      map[string]bool{probeDirectory: true},
		denied:       map[string]bool{},
		outputs:      map[string]string{listVolumeGroups: "  atlas\n"},
		failing:      map[string]bool{},
		unstartable:  map[string]bool{},
		statuses:     map[string]model.VirtualMachineStatus{},
		observeFail:  map[string]bool{},
		firecracker:  map[string]bool{},
		livenessFail: map[string]bool{},
	}
}

// withRunning adds every artifact a running VM leaves on a host.
func (host *fakeHost) withRunning(uuid string) *fakeHost {
	host.withStopped(uuid)
	network := networkingOf[uuid]
	host.units = append(host.units,
		"firecracker-vm@"+uuid+".service loaded active running Firecracker VM "+uuid)
	host.namespaces = append(host.namespaces, network.namespace+" (id: 0)")
	host.links = append(host.links,
		fmt.Sprintf("5: %s@if4: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500", network.hostVeth))
	host.proxies = append(host.proxies, network.address+" dev eth0 proxy")
	host.firecracker[uuid] = true
	host.outputs[linksIn(network.namespace)] = fmt.Sprintf(
		"1: lo: <LOOPBACK,UP> mtu 65536\n2: %s: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500\n", network.tap,
	)
	host.statuses[uuid] = model.StatusRunning
	return host
}

// withStopped adds the durable artifacts and nothing else. The unit is left out
// on purpose: systemd unloads an inactive template instance, so a stopped VM
// commonly appears in no unit listing at all.
func (host *fakeHost) withStopped(uuid string) *fakeHost {
	host.directories = append(host.directories, uuid)
	host.volumes = append(host.volumes,
		fmt.Sprintf("  atlas-vm-%s,10737418240,pool0,atlas-image-bench", uuid))
	host.present["sudo test -d "+jailRootOf(uuid)] = true
	host.outputs["sudo cat "+environmentPathOf(uuid)] = environmentText(uuid)
	host.statuses[uuid] = model.StatusStopped
	return host
}

// withSleeping is a stopped VM that kept its proxy-NDP entry: park.py leaves the
// /128 answered so an inbound SYN can be trapped and wake it.
func (host *fakeHost) withSleeping(uuid string) *fakeHost {
	host.withStopped(uuid)
	host.proxies = append(host.proxies, networkingOf[uuid].address+" dev eth0 proxy")
	host.statuses[uuid] = model.StatusSleeping
	return host
}

func (host *fakeHost) scan(t *testing.T) (Result, error) {
	t.Helper()
	host.commands = &fakeCommands{
		outputs: host.outputs, present: host.present, denied: host.denied, failing: host.failing,
		unstartable: host.unstartable,
	}
	host.commands.outputs[listDirectories] = strings.Join(host.directories, "\n")
	host.commands.outputs[listUnits] = strings.Join(host.units, "\n")
	host.commands.outputs[listNamespaces] = strings.Join(host.namespaces, "\n")
	host.commands.outputs[listLinks] = strings.Join(host.links, "\n")
	host.commands.outputs[listProxies] = strings.Join(host.proxies, "\n")
	host.commands.outputs[listVolumes] = strings.Join(host.volumes, "\n")
	host.observer = &fakeObserver{statuses: host.statuses, failing: host.observeFail}
	scanner := &Scanner{
		commandsFor: func(*run.Runner) commands { return host.commands },
		observer:    host.observer,
		liveness:    host.liveness,
		clock:       fixedClock{},
	}
	return scanner.Scan(context.Background(), nil)
}

// liveness stands in for internal/fcattach. The scan asks whether a Firecracker
// ANSWERED and not whether its socket file is there, so the fake models the three
// answers that probe really gives: a live process, nothing answering, and a probe
// that could not be made. The commands it would render belong to that package's
// tests; asserting them here as well would be one contract written down twice.
func (host *fakeHost) liveness(
	_ context.Context, _ *run.Runner, uuid string,
) (fcattach.Process, bool, error) {
	if host.livenessFail[uuid] {
		return fcattach.Process{}, false, errCommandFailed
	}
	if !host.firecracker[uuid] {
		return fcattach.Process{}, false, nil
	}
	return fcattach.Process{
		UUID: uuid, Pid: 15843, APISocket: apiSocketOf(uuid), State: fcattach.StateRunning,
	}, true, nil
}

// fakeCommands answers rendered commands from a script and records every one of
// them, so a test can assert not only what Boat concluded but what it asked.
type fakeCommands struct {
	outputs     map[string]string
	present     map[string]bool
	denied      map[string]bool
	failing     map[string]bool
	unstartable map[string]bool
	issued      []string
}

func (fake *fakeCommands) record(command string) { fake.issued = append(fake.issued, command) }

func (fake *fakeCommands) Run(_ context.Context, template string, parameters ...any) (string, error) {
	command := render(template, parameters...)
	fake.record(command)
	if fake.failing[command] {
		return "", errCommandFailed
	}
	return fake.outputs[command], nil
}

// RunUnchecked models the real contract: a non-zero exit is DISCARDED, and the
// error return means the command could not be started at all. A fake that
// errored on a non-zero exit would make an optional enumeration look fatal, and
// the scan would appear to fail in a test for a reason it never fails on a host.
func (fake *fakeCommands) RunUnchecked(
	_ context.Context, template string, parameters ...any,
) (string, error) {
	command := render(template, parameters...)
	fake.record(command)
	if fake.unstartable[command] {
		return "", errCommandFailed
	}
	return fake.outputs[command], nil
}

// Probe answers in three values, like the real one. `denied` outranks `present`
// so that a scenario can say "the artifact IS there and we still could not look",
// which is the shape of the whole bug: a host full of VMs whose probe rule is
// missing must not scan the way an empty host does.
//
// Absent by default: an artifact exists in a scenario only because the scenario
// said so.
func (fake *fakeCommands) Probe(
	_ context.Context, template string, parameters ...any,
) (run.Answer, error) {
	command := render(template, parameters...)
	fake.record(command)
	switch {
	case fake.denied[command]:
		return run.Unknown, errCommandFailed
	case fake.present[command]:
		return run.Yes, nil
	}
	return run.No, nil
}

// render substitutes each {} with its parameter the way run.Render does, minus
// the shell quoting — every value here is a path, a UUID or a device name, and
// an unquoted line is the one a reader can compare to the Python by eye.
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

// fakeObserver stands in for internal/vm. Recording who was observed is how the
// tests check the other half of the quarantine rule: a quarantined UUID is not
// merely kept out of the result, it is never asked about at all.
type fakeObserver struct {
	statuses map[string]model.VirtualMachineStatus
	failing  map[string]bool
	observed []string
}

func (observer *fakeObserver) Observe(
	_ context.Context, _ *run.Runner, uuid string,
) (model.VirtualMachine, error) {
	observer.observed = append(observer.observed, uuid)
	if observer.failing[uuid] {
		return model.VirtualMachine{UUID: uuid, ObservedStatus: model.StatusUnknown}, errCommandFailed
	}
	return model.VirtualMachine{UUID: uuid, ObservedStatus: observer.statuses[uuid]}, nil
}

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Unix(1700000000, 0).UTC() }

// --- assertions --------------------------------------------------------------

func adoptedUUIDs(result Result) []string {
	uuids := make([]string, 0, len(result.VirtualMachines))
	for _, virtualMachine := range result.VirtualMachines {
		uuids = append(uuids, virtualMachine.UUID)
	}
	return uuids
}

func quarantinedUUIDs(result Result) []string {
	uuids := make([]string, 0, len(result.Quarantined))
	for _, record := range result.Quarantined {
		uuids = append(uuids, record.UUID)
	}
	return uuids
}

func assertAdopted(t *testing.T, result Result, expected ...string) {
	t.Helper()
	if got := strings.Join(adoptedUUIDs(result), " "); got != strings.Join(expected, " ") {
		t.Errorf("adopted %q, want %q", got, strings.Join(expected, " "))
	}
}

func assertNotQuarantined(t *testing.T, result Result) {
	t.Helper()
	if len(result.Quarantined) > 0 {
		t.Errorf("quarantined %v, want nothing", quarantinedUUIDs(result))
	}
}

// quarantineOf returns the record for uuid and fails when there is none, or when
// the same UUID also came back as a VM — the two sets must never intersect.
func quarantineOf(t *testing.T, result Result, uuid string) model.Quarantine {
	t.Helper()
	for _, adopted := range adoptedUUIDs(result) {
		if adopted == uuid {
			t.Fatalf("%s was adopted AND quarantined: the two must never intersect", uuid)
		}
	}
	for _, record := range result.Quarantined {
		if record.UUID == uuid {
			return record
		}
	}
	t.Fatalf("%s is not quarantined; quarantined: %v", uuid, quarantinedUUIDs(result))
	return model.Quarantine{}
}

// assertEvidence checks that the record names what was actually wrong. A
// quarantine whose evidence does not name the artifact that disagreed is a
// report an operator cannot act on.
func assertEvidence(t *testing.T, record model.Quarantine, fragments ...string) {
	t.Helper()
	joined := strings.Join(record.Evidence, "\n")
	for _, fragment := range fragments {
		if !strings.Contains(joined, fragment) {
			t.Errorf("evidence does not mention %q:\n%s", fragment, joined)
		}
	}
	if !strings.Contains(joined, record.Reason) {
		t.Errorf("reason %q is not among the evidence:\n%s", record.Reason, joined)
	}
	if record.SeenAt.IsZero() {
		t.Error("SeenAt is zero: a quarantine record is only worth as much as its timestamp")
	}
}
