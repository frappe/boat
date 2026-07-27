// Package run is the only place in Boat that runs a subprocess. Everything else
// is a pure function over strings, and is therefore unit-testable with no host.
//
// It ports Atlas's scripts/lib/atlas/_run.py, which existed to give Python the
// two things `bash -x; set -euo pipefail` gave the shell scripts for free: a
// trace of every command into the Task log, and abort on the first failure.
//
// The trust model is parameterized SQL moved to the shell. A command is a
// literal template with `{}` holes; each hole is filled with a shell-quoted
// parameter, and the finished line is split into an argv and executed with NO
// shell. A value carrying a space, a `;`, a `|`, a `$(…)` or a quote can
// therefore never break out of its slot, and "forgetting to quote" is not
// expressible. Shell is the one deliberate exception, and only for
// metacharacters written in the literal template.
//
// The one deliberate change from the Python: the trace goes to the io.Writer
// handed to NewRunner instead of to this process's stderr, because Boat folds
// it into the operation record that Atlas shows in the Task row. One operation
// gets one Runner, and a Runner is used by one goroutine at a time.
package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// CommandError is a command that exited non-zero. It carries the argv, the
// code and both streams, so the operation record shows exactly what failed —
// the equivalent of `bash -x` stopping at the failing line.
type CommandError struct {
	Argv     []string
	ExitCode int
	Output   string
}

func (commandError *CommandError) Error() string {
	return fmt.Sprintf(
		"command failed (exit %d): %s\n%s",
		commandError.ExitCode, join(commandError.Argv), commandError.Output,
	)
}

// Runner runs commands and writes a `set -x` style trace of them.
type Runner struct{ trace io.Writer }

// NewRunner writes its command trace to trace. A nil trace discards it, which
// is what an always-on poller wants: atlas-wake-trap reads the same counters
// every second, and a per-second trace line is journal noise with no
// diagnostic value.
func NewRunner(trace io.Writer) *Runner { return &Runner{trace: trace} }

// Run renders template, runs it with no shell, and returns its stdout.
//
//	runner.Run(ctx, "sudo systemctl stop nginx")          // literals only
//	runner.Run(ctx, "sudo ip link set {} up", tapDevice)  // auto-quoted
//
// A non-zero exit is a *CommandError. The command's own stderr goes to the
// trace and never into the returned stdout, so a caller that parses output
// (blockdev --getsize64) is never fed diagnostics.
func (runner *Runner) Run(ctx context.Context, template string, parameters ...any) (string, error) {
	return runner.checked(ctx, "", template, parameters)
}

// RunUnchecked is Run with the exit code discarded — the guarded `|| true`.
// It still returns stdout, and errors only when the command could not be run at
// all: a missing binary, or a context that ended first.
func (runner *Runner) RunUnchecked(ctx context.Context, template string, parameters ...any) (string, error) {
	result, _, err := runner.invoke(ctx, "", template, parameters)
	return result.standardOutput, err
}

// OK runs a command purely as a boolean gate — `cmd >/dev/null 2>&1` inside an
// `if`. It never traces, never forwards output and never errors; true means
// exit 0. This is how an existence probe (`lvs <volume>`) ports.
func (runner *Runner) OK(ctx context.Context, template string, parameters ...any) bool {
	argv, err := Render(template, parameters...)
	if err != nil {
		return false
	}
	result, err := execute(ctx, argv, "")
	return err == nil && result.exitCode == 0
}

// Input runs a command with stdin fed to it — the `printf … | sudo cmd` form,
// and the only way a secret (a WireGuard private key) reaches a command without
// passing through the process table and the operation record.
func (runner *Runner) Input(ctx context.Context, stdin string, template string, parameters ...any) (string, error) {
	return runner.checked(ctx, stdin, template, parameters)
}

// Shell runs the rendered template through `sh -c`, so metacharacters written
// in the literal template (`|`, `>`, `*`, `&&`) are honoured — the one thing
// Run deliberately will not do. Parameters still go through the same `{}`
// quoting, so an interpolated value can never inject into the pipeline; only
// the template you wrote is shell. Use it sparingly.
//
//	runner.Shell(ctx, "tail -c +{} {} | zstd -dc -f > {}", offset, packed, kernel)
//
// The rendered line is passed as a single quoted argument, so `sh -c` sees one
// script and Boat still execs exactly one process with no shell of its own.
func (runner *Runner) Shell(ctx context.Context, template string, parameters ...any) (string, error) {
	rendered, err := Substitute(template, parameters...)
	if err != nil {
		return "", err
	}
	return runner.Run(ctx, "sh -c {}", rendered)
}

// checked is Run and Input: a non-zero exit becomes a *CommandError carrying
// both streams, the way `set -e` stopped a script at the failing line.
func (runner *Runner) checked(ctx context.Context, stdin string, template string, parameters []any) (string, error) {
	result, argv, err := runner.invoke(ctx, stdin, template, parameters)
	if err != nil || result.exitCode == 0 {
		return result.standardOutput, err
	}
	return result.standardOutput, &CommandError{
		Argv:     argv,
		ExitCode: result.exitCode,
		Output:   result.standardOutput + result.standardError,
	}
}

// invoke is the shared path: render, trace, run, then fold the command's own
// stderr into the trace. Python wrote that stderr to the process's stderr,
// which is what reached the Task log; here the trace writer is that stream.
func (runner *Runner) invoke(
	ctx context.Context, stdin string, template string, parameters []any,
) (outcome, []string, error) {
	argv, err := Render(template, parameters...)
	if err != nil {
		return outcome{}, nil, err
	}
	startedAt := runner.traceStart(argv)
	result, err := execute(ctx, argv, stdin)
	runner.traceFinish(argv, startedAt)
	runner.write(result.standardError)
	return result, argv, err
}

// traceStart writes the `+ <command>` line before the command runs, so a
// command that hangs is still named in the record.
func (runner *Runner) traceStart(argv []string) time.Time {
	runner.write("+ " + join(argv) + "\n")
	return time.Now()
}

// traceFinish closes the trace with `+ (<elapsed>s) <command>`, putting each
// command's wall-clock cost next to its invocation: that is how a slow host
// step is spotted in a Task row without instrumenting anything.
func (runner *Runner) traceFinish(argv []string, startedAt time.Time) {
	runner.write(fmt.Sprintf("+ (%.3fs) %s\n", time.Since(startedAt).Seconds(), join(argv)))
}

// write is best effort: a trace writer that fails must not fail the command it
// was describing.
func (runner *Runner) write(text string) {
	if runner.trace == nil || text == "" {
		return
	}
	_, _ = io.WriteString(runner.trace, text)
}

// outcome is what one finished subprocess left behind.
type outcome struct {
	standardOutput string
	standardError  string
	exitCode       int
}

// execute runs argv with no shell. A non-zero exit is not an error here, it is
// data in the outcome, so each caller decides whether to raise on it. The error
// return means the command never ran — a missing binary — or that ctx ended,
// which is how a shutting-down daemon avoids leaking subprocesses.
func execute(ctx context.Context, argv []string, stdin string) (outcome, error) {
	if len(argv) == 0 {
		return outcome{}, errors.New("no command to run: the template rendered to nothing")
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var standardOutput, standardError strings.Builder
	command.Stdout, command.Stderr = &standardOutput, &standardError
	command.Stdin = strings.NewReader(stdin)
	err := command.Run()
	result := outcome{standardOutput: standardOutput.String(), standardError: standardError.String()}
	return classify(ctx, result, err)
}

// classify sorts what exec reported: an exit status is an outcome, a cancelled
// context and a failure to start are errors.
func classify(ctx context.Context, result outcome, err error) (outcome, error) {
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.exitCode = exitError.ExitCode()
		return result, nil
	}
	return result, err
}
