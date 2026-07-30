package reservedip

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

var errCommandFailed = errors.New("command failed")

const (
	metaAddress = metadataAnchorBase + "/address"
	metaGateway = metadataAnchorBase + "/gateway"

	anchorAddress = "10.47.0.10"
	anchorGateway = "10.47.0.1"
	reservedIP    = "146.190.11.153"
	guestIP       = "100.64.0.2"
	hostVeth      = "veth-abc"
)

// fakeCommands answers rendered commands from a script and records each one with a
// prefix for how much its failure mattered: "? " for a boolean gate, "- " for a
// discarded exit code, nothing for a command whose failure aborts the verb. The
// sequence is what a port of the Python gets wrong, so it is asserted whole.
type fakeCommands struct {
	outputs map[string]string
	present map[string]bool
	failing map[string]bool
	trace   []string
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{outputs: map[string]string{}, present: map[string]bool{}, failing: map[string]bool{}}
}

func (fake *fakeCommands) output(command, text string) *fakeCommands {
	fake.outputs[command] = text
	return fake
}
func (fake *fakeCommands) exists(command string) *fakeCommands {
	fake.present[command] = true
	return fake
}

func (fake *fakeCommands) record(prefix, command string) {
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

func (fake *fakeCommands) RunUnchecked(_ context.Context, template string, parameters ...any) (string, error) {
	command := render(template, parameters...)
	fake.record("- ", command)
	if fake.failing[command] {
		return "", errCommandFailed
	}
	return fake.outputs[command], nil
}

func (fake *fakeCommands) OK(_ context.Context, template string, parameters ...any) bool {
	command := render(template, parameters...)
	fake.record("? ", command)
	return fake.present[command]
}

// render substitutes {} the way run.Render does minus the shell quoting, so a
// recorded line reads like the Python's and can be compared by eye. A quoted
// literal already in the template (the chain clause) is left exactly as written.
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
		t.Fatalf("command sequence:\ngot:\n  %s\nwant:\n  %s",
			strings.Join(fake.trace, "\n  "), strings.Join(expected, "\n  "))
	}
	for index := range expected {
		if fake.trace[index] != expected[index] {
			t.Errorf("command %d:\ngot:  %s\nwant: %s", index, fake.trace[index], expected[index])
		}
	}
}

func (fake *fakeCommands) issued(fragment string) bool {
	for _, recorded := range fake.trace {
		if strings.Contains(recorded, fragment) {
			return true
		}
	}
	return false
}

const routeShowOutput = `[{"dst":"default","gateway":"10.47.0.1","dev":"eth0"}]`

// A DigitalOcean host, fresh: metadata answers with an anchor, no chain or rule is
// there yet, so the whole sequence installs — DNAT the anchor, SNAT the guest out
// as the anchor, forward-accept, and policy-route the guest via the anchor gateway.
func TestAttachOnADigitalOceanHostInstallsTheAnchorNATAndEgressRoute(t *testing.T) {
	fake := newFakeCommands().
		exists("curl -s --max-time 3 -o /dev/null "+metaAddress).
		output("curl -s --max-time 5 "+metaAddress, anchorAddress+"\n").
		output("curl -s --max-time 5 "+metaGateway, anchorGateway+"\n").
		output("ip -j -4 route show default", routeShowOutput)

	delivery, err := (&applier{commands: fake}).attach(context.Background(), guestIP, hostVeth, reservedIP)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !delivery.Anchored || delivery.Anchor.Address != anchorAddress || delivery.Anchor.Gateway != anchorGateway {
		t.Fatalf("delivery = %+v, want anchored via %s/%s", delivery, anchorAddress, anchorGateway)
	}
	assertTrace(t, fake,
		"? curl -s --max-time 3 -o /dev/null "+metaAddress,
		"curl -s --max-time 5 "+metaAddress,
		"curl -s --max-time 5 "+metaGateway,
		"? sudo nft list chain inet atlas prerouting",
		"sudo nft add chain inet atlas prerouting '{ type nat hook prerouting priority dstnat; policy accept; }'",
		"sudo nft list chain inet atlas prerouting",
		"sudo nft add rule inet atlas prerouting ip daddr 10.47.0.10 dnat to 100.64.0.2",
		"sudo nft list chain inet atlas postrouting",
		"sudo nft insert rule inet atlas postrouting ip saddr 100.64.0.2 snat to 10.47.0.10",
		"sudo nft list chain inet atlas forward",
		"sudo nft add rule inet atlas forward ip daddr 100.64.0.2 oifname veth-abc accept",
		"- ip -j -4 route show default",
		"sudo ip -4 route replace default via 10.47.0.1 dev eth0 table 100",
		"sudo ip -4 rule show",
		"sudo ip -4 rule add from 100.64.0.2 lookup 100",
	)
}

// A Self-Managed host, no anchor: metadata does not answer, so DNAT and SNAT key
// on the reserved IP itself and there is NO egress policy route — the reserved IP
// is genuinely routed to the host.
func TestAttachOnARoutedHostKeysOnTheReservedIPAndSkipsTheEgressRoute(t *testing.T) {
	fake := newFakeCommands() // metadata probe absent → routed model

	delivery, err := (&applier{commands: fake}).attach(context.Background(), guestIP, hostVeth, reservedIP)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if delivery.Anchored {
		t.Fatalf("delivery = %+v, want the routed model", delivery)
	}
	assertTrace(t, fake,
		"? curl -s --max-time 3 -o /dev/null "+metaAddress,
		"? sudo nft list chain inet atlas prerouting",
		"sudo nft add chain inet atlas prerouting '{ type nat hook prerouting priority dstnat; policy accept; }'",
		"sudo nft list chain inet atlas prerouting",
		"sudo nft add rule inet atlas prerouting ip daddr 146.190.11.153 dnat to 100.64.0.2",
		"sudo nft list chain inet atlas postrouting",
		"sudo nft insert rule inet atlas postrouting ip saddr 100.64.0.2 snat to 146.190.11.153",
		"sudo nft list chain inet atlas forward",
		"sudo nft add rule inet atlas forward ip daddr 100.64.0.2 oifname veth-abc accept",
	)
	if fake.issued("ip -4 route replace") || fake.issued("ip -4 rule add") {
		t.Error("the routed model installed an egress policy route it does not need")
	}
}

// A re-attach on a host that already holds the rules adds nothing: the chain
// already exists, and each rule's substring is already in its chain listing.
func TestAttachIsIdempotentWhenTheRulesArePresent(t *testing.T) {
	fake := newFakeCommands().
		exists("curl -s --max-time 3 -o /dev/null "+metaAddress).
		output("curl -s --max-time 5 "+metaAddress, anchorAddress+"\n").
		output("curl -s --max-time 5 "+metaGateway, anchorGateway+"\n").
		exists("sudo nft list chain inet atlas prerouting").
		output("sudo nft list chain inet atlas prerouting", "ip daddr 10.47.0.10 dnat to 100.64.0.2\n").
		output("sudo nft list chain inet atlas postrouting", "ip saddr 100.64.0.2 snat to 10.47.0.10\n").
		output("sudo nft list chain inet atlas forward", "ip daddr 100.64.0.2 oifname veth-abc accept\n").
		output("ip -j -4 route show default", routeShowOutput).
		output("sudo ip -4 rule show", "0:\tfrom all lookup local\n32765:\tfrom 100.64.0.2 lookup 100\n")

	if _, err := (&applier{commands: fake}).attach(context.Background(), guestIP, hostVeth, reservedIP); err != nil {
		t.Fatalf("attach: %v", err)
	}
	for _, forbidden := range []string{"add chain", "add rule", "insert rule", "rule add"} {
		if fake.issued(forbidden) {
			t.Errorf("a second attach issued %q; it must be a no-op", forbidden)
		}
	}
}

// Detach deletes every rule mentioning the guest's v4 by handle, across all three
// chains, and drops the egress rule — best-effort, keyed only on the guest.
func TestDetachDeletesEveryGuestRuleByHandleAndDropsTheEgressRule(t *testing.T) {
	fake := newFakeCommands().
		output("sudo nft -a list chain inet atlas prerouting",
			"ip daddr 10.47.0.10 dnat to 100.64.0.2 # handle 7\n").
		output("sudo nft -a list chain inet atlas postrouting",
			"ip saddr 100.64.0.2 snat to 10.47.0.10 # handle 9\nip saddr 100.64.0.0/16 masquerade # handle 3\n").
		output("sudo nft -a list chain inet atlas forward",
			"ip daddr 100.64.0.2 oifname veth-abc accept # handle 11\n")

	if err := (&applier{commands: fake}).detach(context.Background(), guestIP); err != nil {
		t.Fatalf("detach: %v", err)
	}
	assertTrace(t, fake,
		"- sudo nft -a list chain inet atlas prerouting",
		"- sudo nft delete rule inet atlas prerouting handle 7",
		"- sudo nft -a list chain inet atlas postrouting",
		"- sudo nft delete rule inet atlas postrouting handle 9",
		"- sudo nft -a list chain inet atlas forward",
		"- sudo nft delete rule inet atlas forward handle 11",
		"- sudo ip -4 rule del from 100.64.0.2 lookup 100",
	)
}

// The addresses reach nft rules, so a value that is not one is refused before any
// command runs — the injection guard, at the boundary that renders.
func TestAttachRefusesValuesThatWouldInjectIntoTheRuleset(t *testing.T) {
	for _, testCase := range []struct{ name, guest, veth, reserved string }{
		{"guest carries an nft statement", "100.64.0.2; drop", hostVeth, reservedIP},
		{"reserved is not an IPv4", guestIP, hostVeth, "146.190.11.153 # comment"},
		{"veth carries a space", guestIP, "veth abc", reservedIP},
		{"veth is over-long", guestIP, "veth-abcdefghijklmnop", reservedIP},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakeCommands()
			if _, err := (&applier{commands: fake}).attach(context.Background(), testCase.guest, testCase.veth, testCase.reserved); err == nil {
				t.Fatal("attach accepted a value that would inject")
			}
			if len(fake.trace) != 0 {
				t.Errorf("a refused attach still ran commands:\n  %s", strings.Join(fake.trace, "\n  "))
			}
		})
	}
}
