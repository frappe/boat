// The harness every phase test in this package runs on: a host described as the
// answers its commands give, plus a recorder of every command issued. Nothing here
// needs qemu-nbd, dmsetup, nbd-client, root or a VM.
//
// Commands are spelled out as literal strings in each phase test rather than
// derived, so a template that drifts from the one scripts/migration-*.py renders
// shows up as a failing golden rather than as a host that quietly stops migrating.

package migration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/run"
)

var errCommandFailed = errors.New("command failed")

// The UUID and its derivations, shared across the phase tests. Same UUID
// scripts/lib/atlas/test_park.py and derive_test.go use, so the suites line up.
const (
	testUUID     = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	testBindIPv4 = "203.0.113.7"
	testSource   = "203.0.113.9"
	testVMv6     = "2400:6180:100:d0:0:1:5835:d003"
	testImage    = "debian12"
)

// fakeCommands answers rendered commands from a script and records every one of
// them. A recorded line carries a prefix for how much the command's failure
// mattered: "? " for a boolean gate or a probe, "- " for a discarded exit code,
// "$ " for a shell line, and nothing for a command whose failure aborts the phase.
// The sequence therefore shows not only what ran but which parts were best-effort —
// most of what a port of these scripts gets wrong.
type fakeCommands struct {
	outputs map[string]string
	present map[string]bool
	failing map[string]bool
	probes  map[string]run.Answer
	trace   []string
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{
		outputs: map[string]string{},
		present: map[string]bool{},
		failing: map[string]bool{},
		probes:  map[string]run.Answer{},
	}
}

func (fake *fakeCommands) output(command, text string) *fakeCommands {
	fake.outputs[command] = text
	return fake
}

func (fake *fakeCommands) exists(command string) *fakeCommands {
	fake.present[command] = true
	return fake
}

func (fake *fakeCommands) fails(command string) *fakeCommands {
	fake.failing[command] = true
	return fake
}

// probe scripts a three-valued answer for a Probe call, so a test can say "denied"
// (Unknown) as well as "no".
func (fake *fakeCommands) probe(command string, answer run.Answer) *fakeCommands {
	fake.probes[command] = answer
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

// Shell records the rendered shell line (the payload sh -c would run), so the
// golden shows the glob or command-substitution the phase relied on a shell for.
func (fake *fakeCommands) Shell(_ context.Context, template string, parameters ...any) (string, error) {
	command := render(template, parameters...)
	fake.record("$ ", command)
	if fake.failing[command] {
		return "", errCommandFailed
	}
	return fake.outputs[command], nil
}

// OK defaults to false: an artifact exists in a scenario only because the scenario
// said so, so a probe nobody scripted reads as absent.
func (fake *fakeCommands) OK(_ context.Context, template string, parameters ...any) bool {
	command := render(template, parameters...)
	fake.record("? ", command)
	if answer, scripted := fake.probes[command]; scripted {
		return answer == run.Yes
	}
	return fake.present[command]
}

// Probe answers in three values so a test can say "denied" (Unknown) as well as
// "no". An unscripted command is No when a plain present-map entry decides it, else
// Unknown falls back to the present map read as a two-valued gate.
func (fake *fakeCommands) Probe(_ context.Context, template string, parameters ...any) (run.Answer, error) {
	command := render(template, parameters...)
	fake.record("? ", command)
	if answer, scripted := fake.probes[command]; scripted {
		if answer == run.Unknown {
			return run.Unknown, fmt.Errorf("could not run %s", command)
		}
		return answer, nil
	}
	if fake.failing[command] {
		return run.Unknown, fmt.Errorf("could not run %s", command)
	}
	if fake.present[command] {
		return run.Yes, nil
	}
	return run.No, nil
}

// render substitutes each {} with its parameter the way run.Render does, minus the
// shell quoting — every value in these tests is a path, an address, a device name
// or a number, and an unquoted line is the one a reader compares to the Python by
// eye. It panics on an arity mismatch, catching a miscounted template for free.
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

func assertIssued(t *testing.T, fake *fakeCommands, fragment string) {
	t.Helper()
	for _, recorded := range fake.trace {
		if strings.Contains(recorded, fragment) {
			return
		}
	}
	t.Errorf("expected a command matching %q, issued:\n  %s", fragment, strings.Join(fake.trace, "\n  "))
}

func assertNotIssued(t *testing.T, fake *fakeCommands, fragment string) {
	t.Helper()
	for _, recorded := range fake.trace {
		if strings.Contains(recorded, fragment) {
			t.Errorf("issued %q, want it not to:\n  %s", fragment, strings.Join(fake.trace, "\n  "))
		}
	}
}
