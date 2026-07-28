package sidecar

import "testing"

// The file is written to be sourced by a shell, so it is read like one: a
// comment is not a key, a quoted value is not quoted, and a key nobody wrote is
// empty rather than a guess.
func TestValueReadsTheFileTheWaySourcingItWould(t *testing.T) {
	const text = "# written by provision\n" +
		"VIRTUAL_MACHINE_IPV6=2001:db8::9\n" +
		"\n" +
		`ATLAS_NETNS="atlas-3f2504e04f89"` + "\n" +
		"ATLAS_FC_UID=247312\n"

	for _, testCase := range []struct{ key, want string }{
		{"VIRTUAL_MACHINE_IPV6", "2001:db8::9"},
		{"ATLAS_NETNS", "atlas-3f2504e04f89"},
		{"ATLAS_FC_UID", "247312"},
		{"TAP_DEVICE", ""},
	} {
		if got := Value(text, testCase.key); got != testCase.want {
			t.Errorf("Value(%q) = %q, want %q", testCase.key, got, testCase.want)
		}
	}
}

// A commented-out assignment is a comment. Reading it as a value would park a
// VM's trap on an address the host stopped holding when someone commented it out.
func TestACommentedAssignmentIsNotAValue(t *testing.T) {
	if got := Value("# VIRTUAL_MACHINE_IPV6=2001:db8::9\n", "VIRTUAL_MACHINE_IPV6"); got != "" {
		t.Errorf("read a commented line as a value: %q", got)
	}
}

// An unwritten sidecar — a terminate that got as far as the files — parses to
// nothing rather than failing, so every caller's "" branch is the one that runs.
func TestAnEmptyFileHasNoValues(t *testing.T) {
	if values := Parse(""); len(values) != 0 {
		t.Errorf("Parse(\"\") = %v, want no values", values)
	}
}
