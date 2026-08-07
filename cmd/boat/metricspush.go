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

// pushMetricsOnce gathers one snapshot, groups the batches by the token that
// carries them, and pushes once per distinct token (host and VMs sharing a token
// go in one push). The pushes are bounded and concurrent: distinct tokens mean
// distinct datum sessions, which do not collide.
func (parts *daemonParts) pushMetricsOnce(ctx context.Context) {
	timestamp := time.Now().UTC().Format(time.RFC3339)
	host := metrics.Gather(ctx, run.NewRunner(io.Discard))
	virtualMachines, err := parts.store.ListVirtualMachines()
	if err != nil {
		slog.Warn("datum: could not list virtual machines for metrics", "error", err)
	}
	grouped := metricspush.Collect(timestamp, parts.serverName, host, virtualMachines, metricspush.DefaultRoots())

	// Group every batch by the token that carries it, then push once per distinct
	// token. datum stamps a whole request with the token's resource_id and keeps one
	// ClickHouse session per resource_id, so two concurrent pushes under the SAME
	// token race ("concurrent queries within the same session"). In single-token mode
	// the host and all VMs share one token and collapse to a single push per tick; in
	// per-VM-token mode the tokens differ and push concurrently and safely.
	byToken := map[string][]datum.Sample{}
	appendBatch := func(token string, samples []datum.Sample) {
		if token == "" || len(samples) == 0 {
			return
		}
		byToken[token] = append(byToken[token], samples...)
	}
	appendBatch(parts.datumTokens.HostToken(), grouped.Host)
	for uuid, samples := range grouped.VMs {
		appendBatch(parts.datumTokens.TokenFor(uuid), samples)
	}

	slots := make(chan struct{}, pushConcurrency)
	var waiting sync.WaitGroup
	for token, samples := range byToken {
		waiting.Add(1)
		slots <- struct{}{}
		go func(token string, samples []datum.Sample) {
			defer waiting.Done()
			defer func() { <-slots }()
			if accepted, err := parts.datum.Push(ctx, token, samples); err != nil {
				slog.Warn("datum: could not push metrics", "error", err)
			} else {
				slog.Info("datum: pushed metrics", "samples", len(samples), "accepted", accepted)
			}
		}(token, samples)
	}
	waiting.Wait()
}
