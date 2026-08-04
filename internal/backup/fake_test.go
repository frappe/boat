// The harness every verb test in this package runs on: a host described as the
// answers its commands give, plus a recorder of every command issued. Nothing here
// needs curl, zstd, LVM, root or a VM. Mirrors internal/migration/fake_test.go.

package backup

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
	present map[string]bool
	failing map[string]bool
	trace   []string
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{
		outputs: map[string]string{},
		present: map[string]bool{},
		failing: map[string]bool{},
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

func (fake *fakeCommands) OK(_ context.Context, template string, parameters ...any) bool {
	command := render(template, parameters...)
	fake.record("? ", command)
	return fake.present[command]
}

// Input records the command with its stdin folded in as `<stdin> | <command>`, so
// a golden pins the "<digest>  <path>" line the sha256 verify is fed.
func (fake *fakeCommands) Input(_ context.Context, stdin, template string, parameters ...any) (string, error) {
	command := render(template, parameters...)
	line := stdin + " | " + command
	fake.record("", line)
	if fake.failing[line] {
		return "", errCommandFailed
	}
	return fake.outputs[line], nil
}

func (fake *fakeCommands) InstallDirectory(_ context.Context, destination, mode string) error {
	line := "install-dir " + mode + " " + destination
	fake.record("", line)
	if fake.failing[line] {
		return errCommandFailed
	}
	return nil
}

// render substitutes each {} with its parameter the way run.Render does, minus the
// shell quoting — every value here is a path, a url, a number or a device name.
// It panics on an arity mismatch, catching a miscounted template for free.
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

// indexOf is the position of the first recorded command containing fragment, for
// ordering assertions (the sha256 verify must precede the decompress).
func indexOf(t *testing.T, fake *fakeCommands, fragment string) int {
	t.Helper()
	for index, recorded := range fake.trace {
		if strings.Contains(recorded, fragment) {
			return index
		}
	}
	t.Fatalf("no command matching %q was issued:\n  %s", fragment, strings.Join(fake.trace, "\n  "))
	return -1
}
