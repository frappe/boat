package main

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/update"
)

// defaultUpdateKeyPath is where an operator provisions the ONE ed25519 public key
// this host trusts to sign its updates. An absent file means self-update is not
// enabled here (see internal/update/key.go), not that some baked-in key is used.
const defaultUpdateKeyPath = "/etc/boat/update-key.pub"

// updateApply is the detached half of a self-update (spec/33 §5), started as
// `boat update-apply --release <dir>` by the daemon's POST /v1/update handler in
// its OWN systemd scope — outside boat.service's cgroup, so `systemctl restart
// boat.service` does not SIGTERM it mid-swap. It reads the release the daemon
// staged, RE-verifies it (defence in depth on the bytes about to be renamed over
// /usr/local/bin/boat), then runs the seven-step Apply: quiesce the running daemon
// over its socket, swap keeping N-1, restart onto the new binary, health-check it,
// and roll back to N-1 on any failure.
func updateApply(arguments []string, errorOutput io.Writer) int {
	releaseDir, keyPath, socket := "", defaultUpdateKeyPath, socketPath()
	for index := 0; index+1 < len(arguments); index += 2 {
		switch arguments[index] {
		case "--release":
			releaseDir = arguments[index+1]
		case "--update-key-file":
			keyPath = arguments[index+1]
		case "--socket":
			socket = arguments[index+1]
		}
	}
	if releaseDir == "" {
		return usage(errorOutput)
	}
	release, err := update.ReadStaged(releaseDir)
	if err != nil {
		return reportError(errorOutput, err)
	}
	trusted, err := update.LoadTrustedKey(keyPath)
	if err != nil {
		return reportError(errorOutput, fmt.Errorf("load the update key: %w", err))
	}
	runner := run.NewRunner(errorOutput)
	host := update.NewHost(
		&socketQuiescer{client: update.UnixSocketClient(socket)},
		runner,
		update.UnixSocketClient(socket),
		update.DefaultUnits(),
	)
	if err := update.Apply(context.Background(), runner, host, release, trusted); err != nil {
		return reportError(errorOutput, err)
	}
	return exitSuccess
}

// socketQuiescer is the updater's update.Quiescer: it asks the STILL-RUNNING
// daemon to quiesce (and, on an abort before the swap, resume) over its local
// socket. That socket is authenticated by unix peer credentials, not a token (see
// SocketHandler), so the boat-user updater needs no secret to drive it.
type socketQuiescer struct{ client *http.Client }

func (quiescer *socketQuiescer) Quiesce(ctx context.Context) error {
	return quiescer.post(ctx, "/v1/quiesce")
}

func (quiescer *socketQuiescer) Resume(ctx context.Context) error {
	return quiescer.post(ctx, "/v1/resume")
}

// post drives one quiesce/resume call. The host in the URL is ignored — the client
// dials the unix socket regardless — so it is a fixed placeholder.
func (quiescer *socketQuiescer) post(ctx context.Context, path string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://boat"+path, nil)
	if err != nil {
		return err
	}
	response, err := quiescer.client.Do(request)
	if err != nil {
		return fmt.Errorf("POST %s to the daemon socket: %w", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s returned %s", path, response.Status)
	}
	return nil
}
