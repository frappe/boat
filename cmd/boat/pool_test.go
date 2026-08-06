package main

import (
	"bytes"
	"strings"
	"testing"
)

// `boat pool --help` exits 0 without touching the host, so an operator (and a
// health check) can prove the verb is installed the same way the Task verbs are
// probed — reset.py reads exactly this exit code. Running it for real would drive
// lvs/losetup/lvcreate, so this asserts only the dispatchable surface.
func TestPoolHelpIsDispatchableWithoutTouchingTheHost(t *testing.T) {
	var output, errorOutput bytes.Buffer
	if code := dispatch([]string{"pool", "--help"}, &output, &errorOutput); code != exitSuccess {
		t.Fatalf("boat pool --help exited %d, want %d\n%s", code, exitSuccess, errorOutput.String())
	}
}

// An unknown flag is a failure, not a silent no-op that would then run the pool
// re-assert against a typo. flag.ContinueOnError names the offending flag.
func TestPoolRejectsAnUnknownFlag(t *testing.T) {
	var output, errorOutput bytes.Buffer
	if code := dispatch([]string{"pool", "--wat"}, &output, &errorOutput); code == exitSuccess {
		t.Fatalf("boat pool --wat exited %d, want a failure", code)
	}
	if !strings.Contains(errorOutput.String(), "wat") {
		t.Fatalf("the refusal did not name the bad flag: %q", errorOutput.String())
	}
}
