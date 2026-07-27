// The pure half: the counter name a UUID derives, the nft commands that install
// the trap, and the handle scrape that removes it. Nothing here touches a host,
// which is what lets the exact rendered rule be asserted on a laptop — and a
// rule that drifts silently is a rule that stops trapping.

package park

import (
	"fmt"
	"strings"

	"github.com/frappe/boat/internal/run"
)

// counterPrefix marks the named counters that are ours. It ends in `_` because
// nft identifiers may not contain `-`, which also rules out spelling the UUID
// the way everything else in Boat spells it.
const counterPrefix = "wake_"

// uuidHexDigits is the length of a UUID with its dashes taken out. A counter
// name of any other length is not one this package wrote.
const uuidHexDigits = 32

// CounterName is the named nft counter holding a VM's trapped SYN count.
//
// It is a pure function of the UUID, and that is the whole point: the daemon
// polls a flat list of counter names and has to know which VM each belongs to,
// and deriving the answer means no map is stored, nothing has to be kept in step
// with the host, and a daemon restart or a host reboot loses nothing.
func CounterName(uuid string) string {
	return counterPrefix + strings.ReplaceAll(uuid, "-", "")
}

// UUIDForCounter is CounterName's inverse: the dashed UUID a counter name came
// from, and false when the name is not one of ours.
//
// The check is strict on purpose. Another feature's named counter sharing the
// table must never be read as a VM, because the trap would then try to wake a
// UUID nothing on this host has ever heard of.
func UUIDForCounter(counter string) (string, bool) {
	hex, ours := strings.CutPrefix(counter, counterPrefix)
	if !ours || len(hex) != uuidHexDigits || !isLowerHex(hex) {
		return "", false
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex[0:8], hex[8:12], hex[12:16], hex[16:20], hex[20:32]), true
}

// isLowerHex is deliberately case-sensitive: nft prints the names back exactly
// as they were written, and every name this package writes is lower case, so an
// upper-case one came from somewhere else.
func isLowerHex(value string) bool {
	for index := 0; index < len(value); index++ {
		if strings.IndexByte("0123456789abcdef", value[index]) < 0 {
			return false
		}
	}
	return true
}

// counterCommand adds the VM's named counter. It is a table-scope object rather
// than a rule-scope one, which is what puts it in `nft list counters` — the flat
// surface the poll reads.
func counterCommand(uuid string) string {
	return "add counter inet atlas " + CounterName(uuid)
}

// wakeRuleCommand is the trap: count and DROP a connection-opening TCP SYN
// addressed to this VM's /128.
//
// `tcp flags syn / fin,syn,rst,ack` is nft's mask/value form — SYN set, and FIN,
// RST and ACK clear — so a SYN-ACK returning from somewhere else and any
// mid-stream segment both fall through. Matching TCP flags implies TCP, so ICMP
// and UDP cannot match this rule at all and never wake a VM. drop rather than
// reject, so the client retransmits into the guest a second later instead of
// being told the connection failed.
//
// The address is the only datum here and goes through the same quoting every
// other command's parameters do. The counter name is a derived identifier, not
// data, and stays literal — nft has to parse it as an identifier.
func wakeRuleCommand(uuid string, address string) string {
	return fmt.Sprintf(
		"add rule inet atlas %s ip6 daddr %s tcp flags syn / fin,syn,rst,ack counter name %s drop",
		forwardChain, run.Quote(address), CounterName(uuid),
	)
}

// ruleHandles reads `nft -a list chain`'s output for the handles of the rules
// mentioning this VM's counter.
//
// Deleting by handle is the only way nft removes a rule it did not just add, and
// the counter name is what identifies ours: it is unique per VM, so a line
// carrying it is this VM's trap and nothing else's. Every match is returned
// rather than the first, so a chain that somehow ended up with two copies is
// left with none.
func ruleHandles(listing string, counter string) []string {
	var handles []string
	for line := range strings.Lines(listing) {
		if !strings.Contains(line, counter) || !strings.Contains(line, "handle") {
			continue
		}
		if fields := strings.Fields(line); len(fields) > 0 {
			handles = append(handles, fields[len(fields)-1])
		}
	}
	return handles
}
