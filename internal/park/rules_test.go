package park

import (
	"strings"
	"testing"

	"github.com/frappe/boat/internal/run"
)

func TestCounterNameStripsTheDashesTheIdentifierCannotCarry(t *testing.T) {
	if got := CounterName(testUUID); got != "wake_"+testHex {
		t.Errorf("CounterName = %q, want %q", got, "wake_"+testHex)
	}
}

// The derivation is only useful if it inverts: the poll reads a flat list of
// names and has to know whose counter each one is, with nothing stored.
func TestACounterNameRoundTripsBackToItsUUID(t *testing.T) {
	uuid, ours := UUIDForCounter(CounterName(testUUID))
	if !ours || uuid != testUUID {
		t.Errorf("UUIDForCounter = %q, %v; want %q, true", uuid, ours, testUUID)
	}
}

// A name that is not ours must not yield a UUID. The trap would otherwise try to
// wake a VM this host has never heard of, on the strength of another feature's
// counter sharing the table.
func TestAForeignCounterNameYieldsNoUUID(t *testing.T) {
	for _, name := range []string{
		"bytes_total",
		"wake_short",
		"wake_" + strings.Repeat("z", 32),
		"wake_" + testHex + "0",
		"wake_",
		// nft prints names back exactly as they were written, and everything
		// this package writes is lower case.
		"wake_" + strings.ToUpper(testHex),
	} {
		if uuid, ours := UUIDForCounter(name); ours {
			t.Errorf("UUIDForCounter(%q) = %q, want no UUID", name, uuid)
		}
	}
}

func TestTheCounterIsATableScopeNamedObject(t *testing.T) {
	// Table scope is what puts it in `nft list counters`, the flat surface the
	// per-second poll reads; an anonymous rule counter appears nowhere.
	assertArgv(t, counterCommand(testUUID),
		"add", "counter", "inet", "atlas", "wake_"+testHex,
	)
}

func TestTheWakeRuleCountsAndDropsAConnectionOpeningSYN(t *testing.T) {
	assertArgv(t, wakeRuleCommand(testUUID, testAddress),
		"add", "rule", "inet", "atlas", "forward",
		"ip6", "daddr", testAddress,
		"tcp", "flags", "syn", "/", "fin,syn,rst,ack",
		"counter", "name", "wake_"+testHex, "drop",
	)
}

// The two properties the whole trap rests on, asserted as text because that is
// what nft parses: TCP only, and dropped rather than rejected.
func TestTheWakeRuleIsTCPOnlyAndDropsRatherThanRejects(t *testing.T) {
	rule := wakeRuleCommand(testUUID, testAddress)
	if !strings.Contains(rule, "tcp flags syn / fin,syn,rst,ack") {
		t.Errorf("the SYN mask/value match is not in the rule: %s", rule)
	}
	if !strings.HasSuffix(rule, " drop") {
		t.Errorf("the rule does not end in drop, so the client would not retransmit: %s", rule)
	}
	// A reject would hand the client an error to show a user instead of letting
	// its stack retransmit into the woken guest.
	if strings.Contains(rule, "reject") {
		t.Errorf("the rule rejects: %s", rule)
	}
	// Any second protocol clause would mean something other than a TCP SYN could
	// reach this rule's verdict.
	for _, protocol := range []string{"icmp", "icmpv6", "udp", "meta l4proto"} {
		if strings.Contains(rule, protocol) {
			t.Errorf("the rule mentions %s: %s", protocol, rule)
		}
	}
}

// TCP flags, as the header carries them.
const (
	flagFIN = 1 << iota
	flagSYN
	flagRST
	flagPSH
	flagACK
	flagURG
)

// The verdict the rendered rule reaches for one packet. Reading the match out of
// the rule and applying nft's own semantics is what makes "ICMP cannot wake a
// VM" a property of the text that ships, rather than a claim in a comment.
func TestOnlyANewConnectionSYNMatchesTheWakeRule(t *testing.T) {
	rule := wakeRuleCommand(testUUID, testAddress)
	for _, packet := range []struct {
		name     string
		protocol string
		flags    uint8
		matches  bool
	}{
		{"a client opening a connection", "tcp", flagSYN, true},
		{"a SYN-ACK coming back", "tcp", flagSYN | flagACK, false},
		{"a bare ACK mid-stream", "tcp", flagACK, false},
		{"data mid-stream", "tcp", flagPSH | flagACK, false},
		{"a connection being torn down", "tcp", flagFIN | flagACK, false},
		{"a reset", "tcp", flagRST, false},
		{"a SYN-FIN scan", "tcp", flagSYN | flagFIN, false},
		// The "TCP only" contract: these have no TCP header at all, so the
		// rule's match cannot apply to them. They fall through the chain's
		// accept policy, are forwarded out the dummy, and die there.
		{"a ping", "icmpv6", 0, false},
		{"a UDP datagram", "udp", 0, false},
	} {
		if got := matchesWakeRule(t, rule, packet.protocol, packet.flags); got != packet.matches {
			t.Errorf("%s: matched=%v, want %v", packet.name, got, packet.matches)
		}
	}
}

// matchesWakeRule applies the rule's `tcp flags <value> / <mask>` match to one
// packet, the way nft does: the match reads the TCP header, so a packet that is
// not TCP cannot satisfy it at all, and a packet that is matches exactly when
// (flags & mask) == value.
func matchesWakeRule(t *testing.T, rule string, protocol string, flags uint8) bool {
	t.Helper()
	value, mask := flagsMatch(t, rule)
	if protocol != "tcp" {
		return false
	}
	return flags&mask == value
}

// flagsMatch reads the rule's mask/value form. A rule missing it fails the test
// rather than being read as matching everything: the match IS the contract.
func flagsMatch(t *testing.T, rule string) (value uint8, mask uint8) {
	t.Helper()
	_, rest, found := strings.Cut(rule, "tcp flags ")
	if !found {
		t.Fatalf("the rule has no tcp flags match: %s", rule)
	}
	fields := strings.Fields(rest)
	if len(fields) < 3 || fields[1] != "/" {
		t.Fatalf("the flags match is not nft's mask/value form: %q", rest)
	}
	return flagBits(t, fields[0]), flagBits(t, fields[2])
}

func flagBits(t *testing.T, names string) uint8 {
	t.Helper()
	var bits uint8
	for _, name := range strings.Split(names, ",") {
		bit, known := map[string]uint8{
			"fin": flagFIN, "syn": flagSYN, "rst": flagRST,
			"psh": flagPSH, "ack": flagACK, "urg": flagURG,
		}[name]
		if !known {
			t.Fatalf("the rule names a TCP flag nft would not accept: %q", name)
		}
		bits |= bit
	}
	return bits
}

// The address is the only datum in the rule and goes through the same quoting
// every other parameter does; the counter name is a derived identifier and stays
// literal, because nft has to parse it as one.
func TestTheWakeRuleQuotesTheAddressAndLeavesTheCounterNameLiteral(t *testing.T) {
	rule := wakeRuleCommand(testUUID, "2001:db8::1")
	argv, err := run.Split(rule)
	if err != nil {
		t.Fatalf("the rule does not parse as a command line: %v", err)
	}
	if count := countArgv(argv, "wake_"+testHex); count != 1 {
		t.Errorf("the counter name appears %d times in the argv, want 1: %v", count, argv)
	}
	// A value carrying shell metacharacters cannot break out of its slot; it
	// stays exactly one argument, and an invalid one is nft's to refuse.
	hostile := wakeRuleCommand(testUUID, "2001:db8::1; nft flush ruleset")
	argv, err = run.Split(hostile)
	if err != nil {
		t.Fatalf("the rule does not parse as a command line: %v", err)
	}
	if countArgv(argv, "2001:db8::1; nft flush ruleset") != 1 {
		t.Errorf("the address did not survive as exactly one argument: %v", argv)
	}
}

func TestRuleHandlesFindsOnlyThisVirtualMachinesRules(t *testing.T) {
	listing := "table inet atlas {\n" +
		"  chain forward {\n" +
		"    ip6 daddr " + otherAddress + " counter packets 4 bytes 300 accept # handle 7\n" +
		"    ip6 daddr " + testAddress + " tcp flags syn / fin,syn,rst,ack" +
		" counter name wake_" + testHex + " drop # handle 42\n" +
		"    ip6 daddr " + otherAddress + " tcp flags syn / fin,syn,rst,ack" +
		" counter name wake_" + otherHex + " drop # handle 43\n" +
		"  }\n}\n"
	handles := ruleHandles(listing, "wake_"+testHex)
	if len(handles) != 1 || handles[0] != "42" {
		t.Errorf("ruleHandles = %v, want [42]", handles)
	}
}

// A chain that somehow ended up with two copies of a VM's rule is left with
// none: two rules would split the packet count across two entries, and the poll
// reads one of them.
func TestRuleHandlesReturnsEveryDuplicate(t *testing.T) {
	line := "    ip6 daddr " + testAddress + " counter name wake_" + testHex + " drop # handle "
	handles := ruleHandles(line+"42\n"+line+"55\n", "wake_"+testHex)
	if len(handles) != 2 || handles[0] != "42" || handles[1] != "55" {
		t.Errorf("ruleHandles = %v, want [42 55]", handles)
	}
}

func TestRuleHandlesFindsNothingForAVirtualMachineThatWasNeverParked(t *testing.T) {
	if handles := ruleHandles("", "wake_"+testHex); handles != nil {
		t.Errorf("ruleHandles = %v, want none", handles)
	}
}

func assertArgv(t *testing.T, command string, expected ...string) {
	t.Helper()
	argv, err := run.Split(command)
	if err != nil {
		t.Fatalf("%q does not parse as a command line: %v", command, err)
	}
	if strings.Join(argv, "\x00") != strings.Join(expected, "\x00") {
		t.Errorf("argv:\ngot:  %v\nwant: %v", argv, expected)
	}
}

func countArgv(argv []string, value string) int {
	count := 0
	for _, argument := range argv {
		if argument == value {
			count++
		}
	}
	return count
}
