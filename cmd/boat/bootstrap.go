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
// The trace goes to stderr so an operator watches each `+ command` as it runs,
// the same live log the Python bootstrap printed.
func bootstrapCommand(arguments []string, errorOutput io.Writer) int {
	if len(arguments) != 0 {
		return usage(errorOutput)
	}
	runner := run.NewRunner(errorOutput)
	if err := bootstrap.Run(context.Background(), runner); err != nil {
		return reportError(errorOutput, err)
	}
	_, _ = io.WriteString(os.Stdout, "bootstrap complete: host is VM-ready\n")
	return exitSuccess
}
