package run

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// These run real subprocesses, because the thing under test is exactly what a
// real one leaves behind: an exit code and whether it complained. A fake would
// answer whatever the test seeded and would therefore prove nothing about the
// distinction this file exists to draw.

// The three shapes measured on a live host, reproduced with /bin/sh so they need
// no sudoers and no root. The middle one is the case the fleet is in whenever a
// probe's allow-list line is missing.
func TestProbeTellsANoApartFromAFailureToLook(t *testing.T) {
	for _, test := range []struct {
		name   string
		script string
		want   Answer
		fails  bool
	}{
		{name: "the host said yes", script: "exit 0", want: Yes},
		{name: "the host said no", script: "exit 1", want: No},
		{
			name:   "sudo refused, which shares its exit code with a no",
			script: "echo 'sudo: a password is required' >&2; exit 1",
			want:   Unknown, fails: true,
		},
		{
			name:   "the command was not there at all, which sh reports as 127",
			script: "no-such-binary-on-any-host",
			want:   Unknown, fails: true,
		},
		{
			name:   "a probe may print to stdout and still be answering no",
			script: "echo chatty; exit 1",
			want:   No,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			answer, err := NewRunner(nil).Probe(context.Background(), "/bin/sh -c {}", test.script)
			if answer != test.want {
				t.Errorf("Probe = %s, want %s", answer, test.want)
			}
			if (err != nil) != test.fails {
				t.Errorf("Probe error = %v, want an error: %v", err, test.fails)
			}
		})
	}
}

// A missing binary never reaches an exit code: exec fails to start it, and that
// is a failure to look however hard it resembles a negative answer.
func TestProbeReportsACommandThatCouldNotBeStarted(t *testing.T) {
	answer, err := NewRunner(nil).Probe(context.Background(), "/nonexistent/binary")
	if answer != Unknown || err == nil {
		t.Fatalf("Probe = %s, %v; want Unknown and an error", answer, err)
	}
}

// A shutting-down daemon must not record "that VM is not asleep" on its way out.
func TestProbeReportsACancelledContextRatherThanAnswering(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	answer, err := NewRunner(nil).Probe(ctx, "/bin/sleep {}", "30")

	if answer != Unknown {
		t.Errorf("Probe = %s, want Unknown", answer)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Probe error = %v, want the context's error", err)
	}
}

// Unknown is the zero value so that a probe nobody ran cannot read as a negative
// the host never gave.
func TestTheZeroAnswerIsCouldNotLook(t *testing.T) {
	var answer Answer
	if answer != Unknown {
		t.Errorf("the zero Answer is %s, want %s", answer, Unknown)
	}
}

// The trace is what an operator reads afterwards, and a denied probe is the
// event most worth having in it: it is the difference between a host that holds
// nothing and a host we were not allowed to look at.
func TestProbeTracesTheDenialThatOKUsedToSwallow(t *testing.T) {
	trace := &bytes.Buffer{}

	_, err := NewRunner(trace).Probe(
		context.Background(), "/bin/sh -c {}", "echo 'sudo: a password is required' >&2; exit 1",
	)

	if err == nil {
		t.Fatal("Probe returned no error for a denied command")
	}
	if !strings.Contains(trace.String(), "a password is required") {
		t.Errorf("trace = %q, want the denial in it", trace.String())
	}
	if !strings.Contains(err.Error(), "a password is required") {
		t.Errorf("error = %v, want it to carry what the command said", err)
	}
}
