package main

import (
	"context"
	"io"
	"os"

	"github.com/frappe/boat/internal/bootstrap"
	"github.com/frappe/boat/internal/run"
)

// bootstrapCommand brings the host it runs on to VM-ready — one command, no SSH,
// no Python, boat driving every privileged step itself. This is the dogfood:
// a bare Ubuntu host becomes able to run Firecracker microVMs because boat, not an
// external controller, installed Firecracker, created the LVM pool, laid the nft
// scaffold, and made the directories.
//
// It is also a Task verb, and takes the Python BootstrapInputs' two flags for
// that reason: Atlas runs `boat bootstrap --firecracker-version … --architecture
// …` where it used to run `atlas bootstrap-server` with the same two, and reads
// back the same one ATLAS_RESULT= line. Same job, same contract — which is what
// lets the cutover be the first word of the command and nothing else. Both flags
// default (see bootstrap.Params), so the by-hand form above still works.
//
// The trace goes to stderr so an operator watches each `+ command` as it runs,
// the same live log the Python bootstrap printed; the result line goes to stdout,
// where the controller's TaskResult.parse reads it off the Task's output.
func bootstrapCommand(arguments []string, errorOutput io.Writer) int {
	flags := newTaskFlags("bootstrap", errorOutput)
	firecrackerVersion := flags.text("firecracker-version", bootstrap.DefaultFirecrackerVersion)
	architecture := flags.text("architecture", "")
	if err := flags.parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	runner := run.NewRunner(errorOutput)
	result, err := bootstrap.Host(context.Background(), runner, bootstrap.Params{
		FirecrackerVersion: *firecrackerVersion,
		Architecture:       *architecture,
	})
	if err != nil {
		return reportError(errorOutput, err)
	}
	// os.Stdout rather than a passed-in writer: main.go hands this verb only the
	// error stream, and the result line has one destination — the Task's stdout —
	// exactly as the completion line below always has.
	if code := emit(os.Stdout, errorOutput, result.Fields()); code != exitSuccess {
		return code
	}
	_, _ = io.WriteString(os.Stdout, "bootstrap complete: host is VM-ready\n")
	return exitSuccess
}
