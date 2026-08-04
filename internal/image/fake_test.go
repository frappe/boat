// The harness every test in this package runs on: a host described as the answers
// its commands give, plus a recorder of every command issued. Nothing here needs
// curl, an LVM stack, root, or a VM. Mirrors internal/migration's fake so a
// template that drifts from the one sync-image.py / promote-snapshot-image.py
// renders shows up as a failing golden rather than a host that quietly stops.

package image

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

var errCommandFailed = errors.New("command failed")

// fakeCommands answers rendered commands from a script and records every one. A
// recorded line carries a prefix for how the command was issued: nothing for Run,
// "- " for RunUnchecked, "$ " for Shell, "? " for OK. Input records the piped
// stdin as `< <stdin> | <command>`; InstallFile/InstallDirectory record as
// `install <mode> <dest>` / `installdir <mode> <dest>`, with InstallFile's content
// kept in a side map so a test can assert exactly what was baked into a file.
type fakeCommands struct {
	outputs  map[string]string
	present  map[string]bool
	failing  map[string]bool
	contents map[string]string
	trace    []string
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{
		outputs:  map[string]string{},
		present:  map[string]bool{},
		failing:  map[string]bool{},
		contents: map[string]string{},
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

// Shell records the rendered shell line (the payload sh -c would run), so the
// golden shows the glob or redirect the step relied on a shell for.
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
	return fake.present[command]
}

// Input records the stdin next to the command so the golden shows the checksum
// manifest fed to `sha256sum -c -`. Failure is keyed on the command alone.
func (fake *fakeCommands) Input(_ context.Context, stdin string, template string, parameters ...any) (string, error) {
	command := render(template, parameters...)
	fake.record("", "< "+stdin+" | "+command)
	if fake.failing[command] {
		return "", errCommandFailed
	}
	return fake.outputs[command], nil
}

func (fake *fakeCommands) InstallFile(_ context.Context, content string, destination string, mode string) error {
	fake.record("", "install "+mode+" "+destination)
	fake.contents[destination] = content
	return nil
}

func (fake *fakeCommands) InstallDirectory(_ context.Context, destination string, mode string) error {
	fake.record("", "installdir "+mode+" "+destination)
	return nil
}

// render substitutes each {} with its parameter the way run.Render does, minus the
// shell quoting — every value in these tests is a path, a url, a device name or a
// number, and an unquoted line is the one a reader compares to the Python by eye.
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

// assertInstalled checks the exact content InstallFile baked into destination.
func assertInstalled(t *testing.T, fake *fakeCommands, destination, content string) {
	t.Helper()
	got, ok := fake.contents[destination]
	if !ok {
		t.Errorf("no file installed at %s", destination)
		return
	}
	if got != content {
		t.Errorf("content at %s:\ngot:  %q\nwant: %q", destination, got, content)
	}
}
