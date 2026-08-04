package main

import (
	"context"
	"flag"
	"io"
	"os/signal"
	"syscall"

	"github.com/frappe/boat/internal/networkd"
	"github.com/frappe/boat/internal/run"
)

// networkd runs the ANCP network-control-plane daemon: the wg-mesh gossip control
// plane on hashicorp/memberlist (spec/31, WO-5). It is a SEPARATE long-running unit
// from `boat daemon` — it needs root/CAP_NET_ADMIN for wg/ip, whereas the API daemon
// runs as the unprivileged `boat` user — so it dispatches here and never over the
// API socket. This mirrors daemon.go's serve/SIGTERM shape: build once, run until the
// service manager signals a stop, then tear down gracefully (memberlist Leave +
// STOPPING=1 inside Daemon.Run).
func networkdCommand(arguments []string, errorOutput io.Writer) int {
	flags := flag.NewFlagSet("boat networkd", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	if err := flags.Parse(arguments); err != nil {
		return exitUsage
	}

	// systemd sends SIGTERM; an operator running it by hand sends SIGINT. Either
	// cancels the context, which drops Daemon.Run into its graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The command trace (every wg/ip line) goes to stderr, which systemd journals —
	// the daemon's equivalent of the operation record `boat daemon` folds into a Task.
	runner := run.NewRunner(errorOutput)
	daemon, err := networkd.Build(ctx, networkd.DefaultConfig(), runner)
	if err != nil {
		return reportError(errorOutput, err)
	}
	if err := daemon.Run(ctx); err != nil {
		return reportError(errorOutput, err)
	}
	return exitSuccess
}
