package main

import (
	"io"
	"testing"
)

// Every served host verb must be wired into Run, or POST /host-verbs/{verb} would
// pass Serves at the boundary and then fall through to exitUsage inside the turn —
// a verb the endpoint advertises but cannot run. `--help` proves dispatch without
// touching the host: the flag parser short-circuits to flag.ErrHelp, which
// reportError turns into a 0 exit, exactly the property reset.py leans on to test
// a host's CLI is live. An unknown verb returns exitUsage instead, so this catches
// a servedHostVerbs entry whose case was never added.
func TestEveryServedHostVerbIsDispatchable(t *testing.T) {
	runner := hostVerbRunner{}
	for verb := range servedHostVerbs {
		if code := runner.Run(verb, []string{"--help"}, io.Discard, io.Discard); code != exitSuccess {
			t.Errorf("Run(%q, --help) = %d, want %d — the verb is served but not dispatched", verb, code, exitSuccess)
		}
	}
}

// A verb outside the set is not dispatched. Serves already refuses it at the
// boundary; this holds the other half, that Run does not quietly answer a verb the
// endpoint never advertised.
func TestUnservedVerbIsNotDispatched(t *testing.T) {
	runner := hostVerbRunner{}
	if runner.Serves("bootstrap") {
		t.Error("bootstrap is served over the daemon, but it installs the daemon and must stay out of band")
	}
	if code := runner.Run("bootstrap", []string{"--help"}, io.Discard, io.Discard); code != exitUsage {
		t.Errorf("Run(bootstrap) = %d, want exitUsage — it is not a host-verb-endpoint verb", code)
	}
}
