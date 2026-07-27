package run

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The subprocess tests use ordinary binaries only: nothing here needs root, a
// host with VMs, or a network.

func TestRunReturnsStandardOutput(t *testing.T) {
	output, err := NewRunner(nil).Run(context.Background(), "/bin/echo {}", "hello world")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if output != "hello world\n" {
		t.Errorf("Run = %q, want the value delivered as one argument", output)
	}
}

// TestRunTracesTheCommandAndItsElapsedTime pins the `set -x` shape Atlas shows
// in the Task row: the command before it runs, and the same command with its
// wall-clock cost after.
func TestRunTracesTheCommandAndItsElapsedTime(t *testing.T) {
	trace := &bytes.Buffer{}
	if _, err := NewRunner(trace).Run(context.Background(), "/bin/echo {}", "a b"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(trace.String(), "\n"), "\n")
	if len(lines) != 2 || lines[0] != "+ /bin/echo 'a b'" {
		t.Fatalf("trace = %q", trace.String())
	}
	if !regexp.MustCompile(`^\+ \(\d+\.\d{3}s\) /bin/echo 'a b'$`).MatchString(lines[1]) {
		t.Errorf("closing trace line = %q", lines[1])
	}
}

func TestRunWithANilTraceDiscardsIt(t *testing.T) {
	if _, err := NewRunner(nil).Run(context.Background(), "/bin/echo quiet"); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunReturnsCommandErrorOnNonZeroExit(t *testing.T) {
	_, err := NewRunner(nil).Run(context.Background(), "/bin/sh -c {}", "echo out; echo bad >&2; exit 3")
	var commandError *CommandError
	if !errors.As(err, &commandError) {
		t.Fatalf("Run error = %v, want *CommandError", err)
	}
	if commandError.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", commandError.ExitCode)
	}
	if !strings.Contains(commandError.Output, "out") || !strings.Contains(commandError.Output, "bad") {
		t.Errorf("Output = %q, want both streams", commandError.Output)
	}
	if !strings.HasPrefix(commandError.Error(), "command failed (exit 3): /bin/sh -c ") {
		t.Errorf("Error() = %q", commandError.Error())
	}
}

// TestRunFoldsCommandStandardErrorIntoTheTrace: Python wrote a command's stderr
// to its own stderr, which is what reached the Task log. Here the trace writer
// is that stream, and diagnostics must never leak into the returned stdout that
// a caller parses.
func TestRunFoldsCommandStandardErrorIntoTheTrace(t *testing.T) {
	trace := &bytes.Buffer{}
	output, err := NewRunner(trace).Run(context.Background(), "/bin/sh -c {}", "echo out; echo warned >&2")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if output != "out\n" {
		t.Errorf("stdout = %q, want it free of diagnostics", output)
	}
	if !strings.Contains(trace.String(), "warned") {
		t.Errorf("trace = %q, want the command's stderr folded in", trace.String())
	}
}

func TestRunRejectsAnArityMismatchWithoutRunningAnything(t *testing.T) {
	trace := &bytes.Buffer{}
	if _, err := NewRunner(trace).Run(context.Background(), "/bin/echo {} {}", "only-one"); err == nil {
		t.Fatal("Run with a mismatched template should fail loud")
	}
	if trace.Len() != 0 {
		t.Errorf("trace = %q, want nothing traced for a command that never ran", trace.String())
	}
}

func TestRunRejectsAnEmptyTemplate(t *testing.T) {
	if _, err := NewRunner(nil).Run(context.Background(), "   "); err == nil {
		t.Fatal("a template that renders to nothing should fail")
	}
}

// TestRunHonoursACancelledContext keeps a shutting-down daemon from leaking
// subprocesses: the context ending kills the command and is reported as the
// context's error, not as a command failure.
func TestRunHonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	_, err := NewRunner(nil).Run(ctx, "/bin/sh -c {}", "sleep 30")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error = %v, want the context's error", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 10*time.Second {
		t.Errorf("Run took %s, want it cut short with the context", elapsed)
	}
}

// TestRunUncheckedDiscardsTheExitCode is the guarded `|| true`.
func TestRunUncheckedDiscardsTheExitCode(t *testing.T) {
	output, err := NewRunner(nil).RunUnchecked(context.Background(), "/bin/sh -c {}", "echo out; exit 7")
	if err != nil {
		t.Fatalf("RunUnchecked: %v", err)
	}
	if output != "out\n" {
		t.Errorf("RunUnchecked = %q, want stdout even after a non-zero exit", output)
	}
}

// TestRunUncheckedStillReportsACommandThatCannotStart: a discarded exit code is
// not a licence to hide a missing binary.
func TestRunUncheckedStillReportsACommandThatCannotStart(t *testing.T) {
	if _, err := NewRunner(nil).RunUnchecked(context.Background(), "/nonexistent/binary"); err == nil {
		t.Fatal("RunUnchecked should report a command that never started")
	}
}

func TestOKIsAPureBooleanGate(t *testing.T) {
	trace := &bytes.Buffer{}
	runner := NewRunner(trace)
	if !runner.OK(context.Background(), "/bin/sh -c {}", "exit 0") {
		t.Error("OK on a successful command = false")
	}
	if runner.OK(context.Background(), "/bin/sh -c {}", "echo noisy >&2; exit 1") {
		t.Error("OK on a failing command = true")
	}
	if runner.OK(context.Background(), "/bin/echo {} {}", "arity") {
		t.Error("OK on an unrenderable template = true")
	}
	if trace.Len() != 0 {
		t.Errorf("trace = %q, want a gate to stay silent", trace.String())
	}
}

func TestInputFeedsStandardInput(t *testing.T) {
	output, err := NewRunner(nil).Input(context.Background(), "secret key\n", "/bin/cat")
	if err != nil {
		t.Fatalf("Input: %v", err)
	}
	if output != "secret key\n" {
		t.Errorf("Input = %q", output)
	}
}

// TestShellHonoursTemplateMetacharactersButNotParameterOnes is the exact line
// Shell exists to draw: the pipeline you wrote is shell, the value you passed
// never is.
func TestShellHonoursTemplateMetacharactersButNotParameterOnes(t *testing.T) {
	runner := NewRunner(nil)
	output, err := runner.Shell(context.Background(), "echo {} | tr a-z A-Z", "hello world")
	if err != nil {
		t.Fatalf("Shell: %v", err)
	}
	if output != "HELLO WORLD\n" {
		t.Errorf("Shell = %q, want the template's pipe honoured", output)
	}
	output, err = runner.Shell(context.Background(), "echo {}", "$(id) && reboot")
	if err != nil {
		t.Fatalf("Shell: %v", err)
	}
	if output != "$(id) && reboot\n" {
		t.Errorf("Shell = %q, want the parameter left inert", output)
	}
}

// fakeCommands makes a fresh directory the first entry on PATH for the duration
// of the test, so a test can stand in for a command it must not really run
// (sudo) or that the machine need not have (curl).
func fakeCommands(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	return directory
}

// fakeCommand writes an executable shell script called name into directory.
func fakeCommand(t *testing.T, directory string, name string, script string) {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("writing fake %s: %v", name, err)
	}
}

// recordingCommand is a fake that appends its working directory and each of its
// arguments, one per line, to record — the shape the install and Firecracker
// tests assert against.
func recordingCommand(record string) string {
	return "{ pwd; printf '%s\\n' \"$@\"; } > '" + record + "'"
}

// recorded reads back what recordingCommand wrote.
func recorded(t *testing.T, record string) []string {
	t.Helper()
	content, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("reading the recorded invocation: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
}
