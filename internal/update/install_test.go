package update

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// recorder is the commands seam described as the sequence it records — no host, no
// sudo, no filesystem. InstallFile is recorded by destination and byte count (not
// the megabytes themselves) so the golden reads like an operator's journal.
type recorder struct {
	trace   []string
	failing map[string]bool
}

func newRecorder() *recorder { return &recorder{failing: map[string]bool{}} }

func (r *recorder) Run(_ context.Context, template string, parameters ...any) (string, error) {
	command := render(template, parameters...)
	r.trace = append(r.trace, command)
	if r.failing[command] {
		return "", errors.New("command failed")
	}
	return "", nil
}

func (r *recorder) InstallFile(_ context.Context, content, destination, mode string) error {
	command := fmt.Sprintf("install -m %s %s (%d bytes)", mode, destination, len(content))
	r.trace = append(r.trace, command)
	if r.failing[command] {
		return errors.New("install failed")
	}
	return nil
}

// render fills a run-style template's {} holes plainly enough for the assertions
// here; the real quoting lives in run.Substitute and is exercised by run's tests.
func render(template string, parameters ...any) string {
	for _, parameter := range parameters {
		template = strings.Replace(template, "{}", fmt.Sprint(parameter), 1)
	}
	return template
}

func TestInstallStagesKeepsNMinusOneThenSwapsAtomically(t *testing.T) {
	r := newRecorder()
	if err := Install(context.Background(), r, []byte("new boat bytes")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	want := []string{
		"install -m 0755 /usr/local/bin/boat.staging (14 bytes)",
		"sudo ln -f /usr/local/bin/boat /usr/local/bin/boat.previous",
		"sudo mv -f /usr/local/bin/boat.staging /usr/local/bin/boat",
	}
	assertTrace(t, r, want)
}

// The N-1 link must be made BEFORE the swap, or a rollback would restore the very
// binary the update is replacing.
func TestInstallKeepsPreviousBeforeSwapping(t *testing.T) {
	r := newRecorder()
	_ = Install(context.Background(), r, []byte("x"))
	ln := indexOf(t, r, "ln -f")
	mv := indexOf(t, r, "mv -f")
	if ln > mv {
		t.Errorf("N-1 link (%d) came after the swap (%d)", ln, mv)
	}
}

// A staging failure must abort before anything is linked or swapped — a host that
// could not even write the new binary must be left running the old one untouched.
func TestInstallAbortsWithoutSwappingOnStageFailure(t *testing.T) {
	r := newRecorder()
	r.failing["install -m 0755 /usr/local/bin/boat.staging (1 bytes)"] = true
	if err := Install(context.Background(), r, []byte("x")); err == nil {
		t.Fatal("Install returned nil despite a staging failure")
	}
	if issued(r, "mv -f") || issued(r, "ln -f") {
		t.Error("Install swapped or linked after failing to stage")
	}
}

func TestRollbackRestoresPrevious(t *testing.T) {
	r := newRecorder()
	if err := Rollback(context.Background(), r); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	assertTrace(t, r, []string{"sudo mv -f /usr/local/bin/boat.previous /usr/local/bin/boat"})
}

func assertTrace(t *testing.T, r *recorder, want []string) {
	t.Helper()
	if len(r.trace) != len(want) {
		t.Fatalf("trace was\n  %s\nwant\n  %s", strings.Join(r.trace, "\n  "), strings.Join(want, "\n  "))
	}
	for i := range want {
		if r.trace[i] != want[i] {
			t.Errorf("step %d = %q, want %q", i, r.trace[i], want[i])
		}
	}
}

func issued(r *recorder, fragment string) bool {
	for _, line := range r.trace {
		if strings.Contains(line, fragment) {
			return true
		}
	}
	return false
}

func indexOf(t *testing.T, r *recorder, fragment string) int {
	t.Helper()
	for i, line := range r.trace {
		if strings.Contains(line, fragment) {
			return i
		}
	}
	t.Fatalf("%q never issued", fragment)
	return -1
}
