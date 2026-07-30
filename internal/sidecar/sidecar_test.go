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

// The wanted strings are what upsert_network_env in network_env.py actually
// returned for these inputs — the writer is held to the Python's rendering byte
// for byte, because a cold boot re-reads exactly this file.
func TestUpsertMatchesThePythonWriter(t *testing.T) {
	for _, testCase := range []struct {
		name, text, key, value, want string
	}{
		{
			name: "append a missing key preserving order",
			text: "IPV4_GUEST_CIDR=100.64.0.2/30\nHOST_VETH=veth-abc\n",
			key:  "RESERVED_IPV4", value: "146.190.11.153",
			want: "IPV4_GUEST_CIDR=100.64.0.2/30\nHOST_VETH=veth-abc\nRESERVED_IPV4=146.190.11.153\n",
		},
		{
			name: "replace an existing key in place",
			text: "IPV4_GUEST_CIDR=100.64.0.2/30\nRESERVED_IPV4=1.1.1.1\nHOST_VETH=x\n",
			key:  "RESERVED_IPV4", value: "2.2.2.2",
			want: "IPV4_GUEST_CIDR=100.64.0.2/30\nRESERVED_IPV4=2.2.2.2\nHOST_VETH=x\n",
		},
		{
			name: "an empty file becomes one line", text: "",
			key: "RESERVED_IPV4", value: "9.9.9.9", want: "RESERVED_IPV4=9.9.9.9\n",
		},
		{
			name: "a file with no trailing newline still ends in one", text: "A=1",
			key: "B", value: "2", want: "A=1\nB=2\n",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := Upsert(testCase.text, testCase.key, testCase.value); got != testCase.want {
				t.Errorf("Upsert(%q, %q, %q) = %q, want %q", testCase.text, testCase.key, testCase.value, got, testCase.want)
			}
		})
	}
}

// remove_network_env's rendering, likewise captured from the Python: the last
// key removed empties the file to "" (not a lone newline), and a commented
// assignment is never a match.
func TestRemoveMatchesThePythonWriter(t *testing.T) {
	for _, testCase := range []struct {
		name, text, key, want string
	}{
		{
			name: "drop one key, keep the rest and their order",
			text: "A=1\nRESERVED_IPV4=1.1.1.1\nB=2\n", key: "RESERVED_IPV4", want: "A=1\nB=2\n",
		},
		{
			name: "removing the only key empties the file",
			text: "RESERVED_IPV4=1.1.1.1\n", key: "RESERVED_IPV4", want: "",
		},
		{
			name: "a commented assignment is not a match",
			text: "A=1\n# RESERVED_IPV4=keepme\nB=2\n", key: "RESERVED_IPV4",
			want: "A=1\n# RESERVED_IPV4=keepme\nB=2\n",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := Remove(testCase.text, testCase.key); got != testCase.want {
				t.Errorf("Remove(%q, %q) = %q, want %q", testCase.text, testCase.key, got, testCase.want)
			}
		})
	}
}

// Upsert then read-back is the round trip attach relies on: the value written is
// the value Parse sees, whatever surrounded it.
func TestUpsertRoundTripsThroughValue(t *testing.T) {
	updated := Upsert("IPV4_GUEST_CIDR=100.64.0.2/30\n", "RESERVED_IPV4", "203.0.113.7")
	if got := Value(updated, "RESERVED_IPV4"); got != "203.0.113.7" {
		t.Errorf("Value after Upsert = %q, want %q", got, "203.0.113.7")
	}
}
