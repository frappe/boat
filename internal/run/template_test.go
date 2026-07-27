package run

import (
	"slices"
	"strings"
	"testing"
)

func TestSubstituteLeavesLiteralsAlone(t *testing.T) {
	rendered, err := Substitute("sudo systemctl stop nginx")
	if err != nil || rendered != "sudo systemctl stop nginx" {
		t.Fatalf("Substitute = %q, %v", rendered, err)
	}
}

func TestSubstituteQuotesOneParameter(t *testing.T) {
	rendered, _ := Substitute("ip link set {} up", "tap0")
	if rendered != "ip link set tap0 up" {
		t.Errorf("Substitute = %q", rendered)
	}
}

func TestSubstituteMakesAValueWithASpaceOneToken(t *testing.T) {
	rendered, _ := Substitute("echo {}", "a b")
	if rendered != "echo 'a b'" {
		t.Errorf("Substitute = %q", rendered)
	}
}

// TestSubstituteLeavesAnNftBraceClauseVerbatim is the whole reason this engine
// is hand-written: nft's brace clauses pass through with zero escaping, which a
// general template engine would have made impossible.
func TestSubstituteLeavesAnNftBraceClauseVerbatim(t *testing.T) {
	clause := "add chain inet atlas forward { type filter hook forward priority filter; policy accept; }"
	if rendered, _ := Substitute(clause); rendered != clause {
		t.Errorf("Substitute = %q, want the braces untouched", rendered)
	}
}

// TestSubstituteTreatsBracesInsideAValueAsData: only the template can contain a
// hole. A `{}` that arrives as a parameter is quoted like anything else, so a
// value can never grow a placeholder of its own.
func TestSubstituteTreatsBracesInsideAValueAsData(t *testing.T) {
	if rendered, _ := Substitute("echo {}", "{}"); rendered != "echo '{}'" {
		t.Errorf("Substitute = %q", rendered)
	}
}

func TestSubstituteRejectsAnArityMismatch(t *testing.T) {
	mismatches := []struct {
		template   string
		parameters []any
	}{
		{"a {} {}", []any{"x"}},
		{"a {}", []any{"x", "y"}},
		{"a", []any{"x"}},
		{"a {}", nil},
	}
	for _, mismatch := range mismatches {
		if rendered, err := Substitute(mismatch.template, mismatch.parameters...); err == nil {
			t.Errorf("Substitute(%q, %q) = %q, want a loud error", mismatch.template, mismatch.parameters, rendered)
		}
	}
}

func TestSubstituteStringifiesANonStringParameter(t *testing.T) {
	if rendered, _ := Substitute("--port {}", 443); rendered != "--port 443" {
		t.Errorf("Substitute = %q", rendered)
	}
}

func TestRenderSplitsIntoAnArgv(t *testing.T) {
	argv, err := Render("sudo ip link set {} up", "tap0")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !slices.Equal(argv, []string{"sudo", "ip", "link", "set", "tap0", "up"}) {
		t.Errorf("Render = %q", argv)
	}
}

func TestRenderKeepsAValueWithASpaceInOneArgvEntry(t *testing.T) {
	argv, _ := Render("a {} b", "x y")
	if !slices.Equal(argv, []string{"a", "x y", "b"}) {
		t.Errorf("Render = %q", argv)
	}
}

// TestRenderPassesAnNftClauseAsOneArgvEntry: the clause reaches nft with its
// braces and semicolons intact, because nothing re-tokenises it after the split.
func TestRenderPassesAnNftClauseAsOneArgvEntry(t *testing.T) {
	clause := "{ type filter hook forward priority filter; policy accept; }"
	argv, _ := Render("nft add chain inet atlas forward {}", clause)
	want := []string{"nft", "add", "chain", "inet", "atlas", "forward", clause}
	if !slices.Equal(argv, want) {
		t.Errorf("Render = %q", argv)
	}
}

// TestRenderSurvivesTheInjectionBattery: an awkward or hostile value cannot
// break out of its slot, become a second argv entry, or reach a shell — there
// is no shell, and the quoting keeps it one token.
func TestRenderSurvivesTheInjectionBattery(t *testing.T) {
	battery := []string{
		"a; rm -rf /",
		"a | tee /etc/passwd",
		"$(whoami)",
		"`id`",
		"a && reboot",
		"' ; echo pwned ; '",
		"../../etc/shadow",
		"a\tb",
		"a\nb",
		"--flag=value with spaces",
		"{ nft brace }",
		`{"state":"Paused"}`,
		"",
		strings.Repeat("'", 5),
	}
	for _, value := range battery {
		argv, err := Render("echo {}", value)
		if err != nil {
			t.Errorf("Render(%q): %v", value, err)
			continue
		}
		if !slices.Equal(argv, []string{"echo", value}) {
			t.Errorf("Render(%q) = %q, want the value as exactly one token", value, argv)
		}
	}
}

func TestRenderHandlesTwoParameters(t *testing.T) {
	argv, _ := Render("ip -6 route replace {}/128 dev {}", "2400:dead::1", "tap0")
	want := []string{"ip", "-6", "route", "replace", "2400:dead::1/128", "dev", "tap0"}
	if !slices.Equal(argv, want) {
		t.Errorf("Render = %q", argv)
	}
}
