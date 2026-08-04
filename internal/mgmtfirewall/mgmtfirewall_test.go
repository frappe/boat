// The harness the three verbs run on: a host described as the answers its commands
// give, plus a recorder of every command issued. Nothing here needs nft,
// systemd-run or root. The ruleset is asserted byte-for-byte because a
// management-firewall that drifts silently either locks the operator out or leaves
// the public side open — both are the failure the confirm/revert dance exists to
// prevent. Mirrors scripts/lib/atlas/test_mgmt_firewall.py.

package mgmtfirewall

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

var errCommandFailed = errors.New("command failed")

type fakeCommands struct {
	outputs map[string]string
	failing map[string]bool
	trace   []string
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{outputs: map[string]string{}, failing: map[string]bool{}}
}

func (fake *fakeCommands) output(command, text string) *fakeCommands {
	fake.outputs[command] = text
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
	return fake.outputs[command], nil
}

// InstallFile records the mode, the whole content (%q) and the destination: for the
// firewall the content IS the behaviour, so a golden that only checked the install
// line would not notice a wrong rule going into the ruleset.
func (fake *fakeCommands) InstallFile(_ context.Context, content, destination, mode string) error {
	fake.record("", fmt.Sprintf("install -m %s %q %s", mode, content, destination))
	return nil
}

func (fake *fakeCommands) InstallDirectory(_ context.Context, destination, mode string) error {
	fake.record("", fmt.Sprintf("install -d -m %s %s", mode, destination))
	return nil
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

// --- pure builder goldens ---------------------------------------------------

func TestMgmtRulesetLocksOnlyThePublicInterface(t *testing.T) {
	got := mgmtRuleset("eth0", 51820, nil)
	want := "table inet atlas_mgmt {\n" +
		"\tchain input {\n" +
		"\t\ttype filter hook input priority filter; policy accept;\n" +
		"\t\tiifname \"eth0\" jump public_input\n" +
		"\t}\n" +
		"\tchain public_input {\n" +
		"\t\tct state established,related accept\n" +
		"\t\tct state invalid drop\n" +
		"\t\tmeta l4proto { icmp, icmpv6 } accept\n" +
		"\t\tudp dport 51820 accept\n" +
		"\t\tdrop\n" +
		"\t}\n" +
		"}\n"
	if got != want {
		t.Errorf("mgmtRuleset:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestMgmtRulesetInsertsAllowPortsBeforeDrop(t *testing.T) {
	got := mgmtRuleset("eth0", 51820, []string{"80", "443"})
	if !strings.Contains(got, "\t\ttcp dport { 80, 443 } accept\n\t\tdrop\n") {
		t.Errorf("allow ports not inserted before drop:\n%s", got)
	}
}

func TestLoadableRulesetPrependsTheReplaceIdiom(t *testing.T) {
	got := loadableRuleset("eth0", 51820, nil)
	prefix := "table inet atlas_mgmt {}\ndelete table inet atlas_mgmt\n"
	if !strings.HasPrefix(got, prefix) {
		t.Errorf("loadableRuleset does not begin with the add-delete idiom:\n%s", got)
	}
	if got != prefix+mgmtRuleset("eth0", 51820, nil) {
		t.Error("loadableRuleset is not the prefix + mgmtRuleset")
	}
}

// --- verb goldens -----------------------------------------------------------

// Apply stages the ruleset, loads it, clears any prior armed revert, and arms a new
// one. The interface is given, so no discovery runs.
func TestApplyLoadsAndArmsTheAutoRevert(t *testing.T) {
	fake := newFakeCommands()
	result, err := Apply(context.Background(), fake, ApplyParams{
		WGPort: 51820, PublicInterface: "eth0", RevertSeconds: 180,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.PublicInterface != "eth0" || result.WGPort != 51820 || result.RevertSeconds != 180 {
		t.Fatalf("result = %+v", result)
	}
	ruleset := loadableRuleset("eth0", 51820, nil)
	assertTrace(t, fake,
		fmt.Sprintf("install -m 0600 %q /run/atlas-mgmt.nft", ruleset),
		"sudo nft -f /run/atlas-mgmt.nft",
		"- sudo systemctl stop atlas-firewall-revert.timer",
		"- sudo systemctl reset-failed atlas-firewall-revert.timer",
		"- sudo systemctl stop atlas-firewall-revert.service",
		"- sudo systemctl reset-failed atlas-firewall-revert.service",
		"sudo systemd-run --collect --on-active=180 --unit=atlas-firewall-revert "+
			"--description=Atlas management-firewall auto-revert (lockout safety) nft delete table inet atlas_mgmt",
	)
}

// With no interface given, Apply discovers it from the default route first.
func TestApplyDiscoversThePublicInterface(t *testing.T) {
	fake := newFakeCommands().output(
		"ip -j route show default", `[{"dst":"default","dev":"ens3"}]`,
	)
	result, err := Apply(context.Background(), fake, ApplyParams{WGPort: 51820, RevertSeconds: 60})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if result.PublicInterface != "ens3" {
		t.Fatalf("discovered interface = %q, want ens3", result.PublicInterface)
	}
	if fake.trace[0] != "ip -j route show default" {
		t.Errorf("first command = %q, want the route discovery", fake.trace[0])
	}
	if !strings.Contains(fake.trace[1], `iifname \"ens3\"`) {
		t.Errorf("the staged ruleset did not lock the discovered interface:\n%s", fake.trace[1])
	}
}

// A host with no default route and no interface given fails loud rather than
// staging an empty-iifname ruleset.
func TestApplyFailsWithNoRouteAndNoInterface(t *testing.T) {
	fake := newFakeCommands().output("ip -j route show default", "[]")
	if _, err := Apply(context.Background(), fake, ApplyParams{WGPort: 51820, RevertSeconds: 60}); err == nil {
		t.Error("expected an error when there is no default route and no interface")
	}
}

// Confirm cancels the armed revert and persists the ruleset + boot unit.
func TestConfirmPersistsAndCancelsTheRevert(t *testing.T) {
	fake := newFakeCommands()
	result, err := Confirm(context.Background(), fake, ConfirmParams{WGPort: 51820, PublicInterface: "eth0"})
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if !result.Confirmed || result.PublicInterface != "eth0" {
		t.Fatalf("result = %+v", result)
	}
	ruleset := loadableRuleset("eth0", 51820, nil)
	assertTrace(t, fake,
		"- sudo systemctl stop atlas-firewall-revert.timer",
		"- sudo systemctl reset-failed atlas-firewall-revert.timer",
		"- sudo systemctl stop atlas-firewall-revert.service",
		"- sudo systemctl reset-failed atlas-firewall-revert.service",
		"install -d -m 0755 /etc/atlas",
		fmt.Sprintf("install -m 0644 %q /etc/atlas/mgmt-firewall.nft", ruleset),
		"- sudo systemctl enable atlas-mgmt-firewall.service",
	)
}

// Revert cancels the armed timer, deletes the live table, and removes the persisted
// ruleset + boot unit. Every step is best-effort.
func TestRevertRestoresOpenAccess(t *testing.T) {
	fake := newFakeCommands()
	result, err := Revert(context.Background(), fake, RevertParams{})
	if err != nil {
		t.Fatalf("Revert: %v", err)
	}
	if !result.Reverted {
		t.Fatal("Revert did not report reverted")
	}
	assertTrace(t, fake,
		"- sudo systemctl stop atlas-firewall-revert.timer",
		"- sudo systemctl reset-failed atlas-firewall-revert.timer",
		"- sudo systemctl stop atlas-firewall-revert.service",
		"- sudo systemctl reset-failed atlas-firewall-revert.service",
		"- sudo nft delete table inet atlas_mgmt",
		"- sudo rm -f /etc/atlas/mgmt-firewall.nft",
		"- sudo systemctl disable atlas-mgmt-firewall.service",
	)
}
