package vmnetwork

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
)

const (
	// The nft chain clauses, passed as parameters so run.Quote makes each the one
	// argv token nft needs (the braces and `;` would otherwise be re-lexed). The
	// forward chain's accept policy is what makes park's "TCP only" wake property
	// fall out; the postrouting chain hooks srcnat for the NAT44 masquerade.
	forwardChainSpecification     = "{ type filter hook forward priority filter; policy accept; }"
	postroutingChainSpecification = "{ type nat hook postrouting priority srcnat; policy accept; }"
)

// step is one command in a fixed sequence, and whether its failure aborts the
// sequence (checked) or is tolerated (unchecked) — the `check=False` the Python
// marks on the idempotent teardown-before-create deletes.
type step struct {
	template  string
	arguments []any
	tolerate  bool
}

func checked(template string, arguments ...any) step {
	return step{template: template, arguments: arguments}
}

func unchecked(template string, arguments ...any) step {
	return step{template: template, arguments: arguments, tolerate: true}
}

// perform runs a sequence in order, stopping at the first checked command that
// fails. A tolerated command's failure is discarded, exactly as the Python's
// `check=False` drops the exit code of a delete of something that was not there.
func perform(ctx context.Context, commands commands, steps []step) error {
	for _, step := range steps {
		if step.tolerate {
			if _, err := commands.RunUnchecked(ctx, step.template, step.arguments...); err != nil {
				return err
			}
			continue
		}
		if _, err := commands.Run(ctx, step.template, step.arguments...); err != nil {
			return err
		}
	}
	return nil
}

func (bringUp *bringUp) perform(ctx context.Context, steps []step) error {
	return perform(ctx, bringUp.commands, steps)
}

// canonicalIPv6 admits only an ordinary IPv6 address and re-emits it in one
// canonical form — the same guard park arms its wake trap behind, applied to
// every value here that reaches an nft rule or an `ip -6` command. A malformed
// address cannot render, because a canonical IPv6 is nothing but lowercase hex
// and colons — no space, no `;`, no `#`.
func canonicalIPv6(address string) (string, bool) {
	parsed, err := netip.ParseAddr(address)
	if err != nil || !parsed.Is6() || parsed.Is4In6() || parsed.Zone() != "" {
		return "", false
	}
	return parsed.String(), true
}

// canonicalIPv4 is canonicalIPv6's v4 twin, for the reserved IP and the guest's
// bare private address.
func canonicalIPv4(address string) (string, bool) {
	parsed, err := netip.ParseAddr(address)
	if err != nil || !parsed.Is4() {
		return "", false
	}
	return parsed.String(), true
}

// canonicalIPv4CIDR admits an IPv4 address-with-prefix, host bits kept — the
// per-VM /30s are host addresses inside their prefix, so the value is not masked.
// Refuses anything that is not an IPv4 prefix before it reaches `ip -4 addr`.
func canonicalIPv4CIDR(cidr string) (string, bool) {
	parsed, err := netip.ParsePrefix(cidr)
	if err != nil || !parsed.Addr().Is4() {
		return "", false
	}
	return parsed.String(), true
}

// validName accepts only what a namespace or interface name can be and a command
// can read as one token: non-empty, and nothing but the characters a name uses.
// It is the device-name twin of canonicalIPv6 — a name carrying a space, a `;` or
// a `#` would inject where it reaches `ip netns`, `ip link` or nft `oifname`.
func validName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		letter := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
		digit := character >= '0' && character <= '9'
		punctuation := character == '-' || character == '_' || character == '.' || character == '@'
		if !letter && !digit && !punctuation {
			return false
		}
	}
	return true
}

// firstRouteDevice reads the `dev` of the first route in `ip -j route show`'s JSON
// array. An empty array is an error, not "": the device is the answer, and an
// absent default route means there is no uplink to bring the VM up against.
func firstRouteDevice(output string) (string, error) {
	var routes []struct {
		Device string `json:"dev"`
	}
	if err := json.Unmarshal([]byte(output), &routes); err != nil {
		return "", err
	}
	if len(routes) == 0 || routes[0].Device == "" {
		return "", errors.New("no default route")
	}
	return routes[0].Device, nil
}

func familyName(family string) string {
	if family == "" {
		return "IPv4"
	}
	return "IPv6"
}

// stripPrefix returns an address-with-prefix's bare address — the guest's /30
// host address for the /32 route and the reserved-ip NAT.
func stripPrefix(cidr string) string {
	address, _, _ := strings.Cut(cidr, "/")
	return address
}
