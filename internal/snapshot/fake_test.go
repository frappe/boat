// The harness every verb test in this package runs on: a host described as the
// answers its commands give, plus a recorder of every command issued. Nothing here
// needs Firecracker, LVM, root or a VM.
//
// Commands are spelled out as literal strings in each verb test rather than
// derived, so a template that drifts from the one scripts/*.py renders shows up as
// a failing golden rather than as a host that quietly stops snapshotting. The
// idiom mirrors internal/migration/fake_test.go.

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

var errCommandFailed = errors.New("command failed")

// The UUID scripts/lib/atlas/test_park.py and internal/migration use, so the
// suites line up.
const testUUID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

// installedFile is one InstallFile call's content and mode, kept so a test can
// assert the bytes staged (host-signature.json) as well as that the call happened.
type installedFile struct {
	content string
	mode    string
}

// fakeCommands answers rendered commands from a script and records every one. A
// recorded line carries a prefix for how much the command's failure mattered:
// "? " for a boolean gate, "- " for a discarded exit code, and nothing for a
// command whose failure aborts the verb. FirecrackerAPI and the install helpers
// record a synthetic line so a golden shows them too.
type fakeCommands struct {
	outputs       map[string]string
	present       map[string]bool
	failing       map[string]bool
	installedFile map[string]installedFile
	trace         []string
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{
		outputs:       map[string]string{},
		present:       map[string]bool{},
		failing:       map[string]bool{},
		installedFile: map[string]installedFile{},
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

// OK defaults to false: an artifact exists in a scenario only because the scenario
// said so, so a probe nobody scripted reads as absent.
func (fake *fakeCommands) OK(_ context.Context, template string, parameters ...any) bool {
	command := render(template, parameters...)
	fake.record("? ", command)
	return fake.present[command]
}

// FirecrackerAPI records the full call — socket dir/name, method, path and body —
// so a golden pins that the pause/resume/create sequence goes to the right socket
// with the right body.
func (fake *fakeCommands) FirecrackerAPI(
	_ context.Context, socketDirectory, socketName, method, apiPath, body string,
) error {
	line := fmt.Sprintf("fcapi %s %s %s %s %s", socketDirectory, socketName, method, apiPath, body)
	fake.record("", line)
	if fake.failing[line] {
		return errCommandFailed
	}
	return nil
}

func (fake *fakeCommands) InstallFile(_ context.Context, content, destination, mode string) error {
	fake.installedFile[destination] = installedFile{content: content, mode: mode}
	fake.record("", "install-file "+mode+" "+destination)
	if fake.failing["install-file "+mode+" "+destination] {
		return errCommandFailed
	}
	return nil
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
// shell quoting — every value here is a path, a number or a device name, and an
// unquoted line is the one a reader compares to the Python by eye. It panics on an
// arity mismatch, catching a miscounted template for free.
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
