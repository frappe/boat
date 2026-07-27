package run

import (
	"slices"
	"testing"
)

// TestQuoteMatchesPythonShlex holds Quote to the oracle: every expectation in
// quoteConformance came out of CPython's shlex.quote for that exact value.
func TestQuoteMatchesPythonShlex(t *testing.T) {
	for _, conformance := range quoteConformance {
		if quoted := Quote(conformance.value); quoted != conformance.quoted {
			t.Errorf("Quote(%q) = %q, shlex.quote said %q", conformance.value, quoted, conformance.quoted)
		}
	}
}

// TestSplitMatchesPythonShlex is the other half of the oracle: shlex.split's
// tokenisation of real rendered command lines, including nft brace clauses,
// Firecracker bodies, and the double-quote escape rules.
func TestSplitMatchesPythonShlex(t *testing.T) {
	for _, conformance := range splitConformance {
		argv, err := Split(conformance.line)
		if err != nil {
			t.Errorf("Split(%q): %v", conformance.line, err)
			continue
		}
		if !slices.Equal(argv, conformance.argv) {
			t.Errorf("Split(%q) = %q, shlex.split said %q", conformance.line, argv, conformance.argv)
		}
	}
}

// TestSplitRejectsUnfinishedQuoting keeps the loud failure: a line whose
// quoting never closes is a rendering bug, and guessing where the token ended
// would run a command nobody wrote.
func TestSplitRejectsUnfinishedQuoting(t *testing.T) {
	for _, line := range splitConformanceErrors {
		if argv, err := Split(line); err == nil {
			t.Errorf("Split(%q) = %q, want an error the way shlex.split raises one", line, argv)
		}
	}
}

// TestQuoteThenSplitReturnsTheValue is the property the whole trust model rests
// on: whatever a parameter contains, it comes back out of the split as exactly
// one argv entry, byte for byte.
func TestQuoteThenSplitReturnsTheValue(t *testing.T) {
	for _, conformance := range quoteConformance {
		argv, err := Split(Quote(conformance.value))
		if err != nil {
			t.Errorf("Split(Quote(%q)): %v", conformance.value, err)
			continue
		}
		if !slices.Equal(argv, []string{conformance.value}) {
			t.Errorf("Split(Quote(%q)) = %q, want one unchanged token", conformance.value, argv)
		}
	}
}

// TestJoinRoundTripsAnArgv covers the trace line: what the operation record
// shows has to be a command line that parses back to the argv that ran.
func TestJoinRoundTripsAnArgv(t *testing.T) {
	argv := []string{"sudo", "nft", "add chain inet atlas forward", "{ policy accept; }", "", "it's"}
	parsed, err := Split(join(argv))
	if err != nil {
		t.Fatalf("Split(join(argv)): %v", err)
	}
	if !slices.Equal(parsed, argv) {
		t.Errorf("Split(join(%q)) = %q", argv, parsed)
	}
}

func TestQuoteLeavesOrdinaryValuesUnquoted(t *testing.T) {
	for _, value := range []string{"simple-path", "/var/lib/atlas", "0644", "tap0", "2400:dead::1/128"} {
		if quoted := Quote(value); quoted != value {
			t.Errorf("Quote(%q) = %q, want it left alone so the trace stays readable", value, quoted)
		}
	}
}

// FuzzQuoteSplitRoundTrip extends the round-trip property past the corpus. It
// runs the seeds on an ordinary `go test`; `go test -fuzz` goes further.
func FuzzQuoteSplitRoundTrip(f *testing.F) {
	for _, conformance := range quoteConformance {
		f.Add(conformance.value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		argv, err := Split(Quote(value))
		if err != nil {
			t.Fatalf("Split(Quote(%q)): %v", value, err)
		}
		if !slices.Equal(argv, []string{value}) {
			t.Errorf("Split(Quote(%q)) = %q, want one unchanged token", value, argv)
		}
	})
}
