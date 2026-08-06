package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/frappe/boat/internal/datum"
	"github.com/frappe/boat/internal/metrics"
	"github.com/frappe/boat/internal/metricspush"
	"github.com/frappe/boat/internal/run"
)

// pushConcurrency bounds how many per-VM ingest POSTs are in flight at once, so a
// slow datum cannot let one tick's fan-out overrun the next.
const pushConcurrency = 8

// pushMetrics collects host and per-VM metrics every interval and pushes them to
// datum, best-effort. It is a resident background loop (registered only when
// --datum-url is set) and returns when ctx is cancelled at shutdown. A failed push
// is logged and the tick moves on: metrics never block or fault the daemon.
func (parts *daemonParts) pushMetrics(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = defaultDatumInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			parts.pushMetricsOnce(ctx)
		}
	}
}

// pushMetricsOnce gathers one snapshot and pushes it: host samples under the host
// token, and each VM's samples under that VM's token, the per-VM pushes bounded and
// concurrent. A VM for which no token has been shipped is skipped.
func (parts *daemonParts) pushMetricsOnce(ctx context.Context) {
	timestamp := time.Now().UTC().Format(time.RFC3339)
	host := metrics.Gather(ctx, run.NewRunner(io.Discard))
	virtualMachines, err := parts.store.ListVirtualMachines()
	if err != nil {
		slog.Warn("datum: could not list virtual machines for metrics", "error", err)
	}
	grouped := metricspush.Collect(timestamp, parts.serverName, host, virtualMachines, metricspush.DefaultRoots())

	if err := parts.datum.Push(ctx, parts.datumTokens.HostToken(), grouped.Host); err != nil {
		slog.Warn("datum: could not push host metrics", "error", err)
	}

	slots := make(chan struct{}, pushConcurrency)
	var waiting sync.WaitGroup
	for uuid, samples := range grouped.VMs {
		token := parts.datumTokens.TokenFor(uuid)
		if token == "" {
			continue
		}
		waiting.Add(1)
		slots <- struct{}{}
		go func(uuid, token string, samples []datum.Sample) {
			defer waiting.Done()
			defer func() { <-slots }()
			if err := parts.datum.Push(ctx, token, samples); err != nil {
				slog.Warn("datum: could not push virtual machine metrics", "uuid", uuid, "error", err)
			}
		}(uuid, token, samples)
	}
	waiting.Wait()
}
