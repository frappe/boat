package main

import (
	"context"
	"io"

	"github.com/frappe/boat/internal/metrics"
	"github.com/frappe/boat/internal/run"
)

// metricsCommand prints the host's metrics in Prometheus text format on stdout —
// CPU/memory capacity, thin-pool fullness, VM counts — measured off the host by
// boat itself. Pointed at a node_exporter textfile collector (or served), it is
// the host's metric feed. The runner's trace is discarded so stdout is nothing but
// the exposition text.
func metricsCommand(arguments []string, output io.Writer, errorOutput io.Writer) int {
	if len(arguments) != 0 {
		return usage(errorOutput)
	}
	runner := run.NewRunner(nil)
	_, _ = io.WriteString(output, metrics.Gather(context.Background(), runner).Render())
	return exitSuccess
}
