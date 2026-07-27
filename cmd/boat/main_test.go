package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/version"
)

func TestUsageListsEveryVerbAndExitsTwo(t *testing.T) {
	unusable := [][]string{
		{},
		{"sail"},
		{"vm"},
		{"vm", "evacuate"},
		{"host"},
		{"host", "rumours"},
	}

	for _, arguments := range unusable {
		var output, errorOutput bytes.Buffer
		if code := dispatch(arguments, &output, &errorOutput); code != exitUsage {
			t.Errorf("%v: got exit %d, want %d", arguments, code, exitUsage)
		}
		for _, verb := range []string{"daemon", "vm start", "vm stop", "vm ls", "vm show", "host facts", "version"} {
			if !strings.Contains(errorOutput.String(), verb) {
				t.Errorf("%v: the usage message does not mention %q", arguments, verb)
			}
		}
	}
}

// `boat version` is the binary's own identity and answers without a daemon: it
// is what an operator reads when the daemon is the thing that will not start.
func TestVersionPrintsThisBuild(t *testing.T) {
	var output, errorOutput bytes.Buffer

	if code := dispatch([]string{"version"}, &output, &errorOutput); code != exitSuccess {
		t.Fatalf("got exit %d, want 0", code)
	}
	if strings.TrimSpace(output.String()) != version.Version {
		t.Errorf("got %q, want %q", output.String(), version.Version)
	}
}

func TestVmVerbsNeedTheirUuidBeforeTheirFlags(t *testing.T) {
	for _, arguments := range [][]string{{"vm", "stop"}, {"vm", "stop", "--graceful=false"}, {"vm", "show"}} {
		var output, errorOutput bytes.Buffer
		if code := dispatch(arguments, &output, &errorOutput); code != exitUsage {
			t.Errorf("%v: got exit %d, want %d", arguments, code, exitUsage)
		}
		if !strings.Contains(errorOutput.String(), "UUID") {
			t.Errorf("%v: the refusal does not say a UUID is missing: %q", arguments, errorOutput.String())
		}
	}
}

// Two runs must never share an operation identifier, or the second would replay
// the first instead of doing its own work.
func TestEachRunMintsItsOwnOperationIdentifier(t *testing.T) {
	first := newOperationIdentifier()
	second := newOperationIdentifier()

	if first == second {
		t.Fatalf("both runs claimed %q", first)
	}
	if !strings.HasPrefix(first, "cli-") {
		t.Errorf("got %q, want an identifier that says where it came from", first)
	}
}
