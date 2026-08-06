package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/frappe/boat/internal/api"
	"github.com/frappe/boat/internal/datum"
	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/token"
	"github.com/frappe/boat/internal/version"
)

const (
	defaultStorePath          = "/var/lib/boat/boat.db"
	defaultTokenFilePath      = "/etc/boat/token"
	defaultDatumTokenFilePath = "/etc/boat/datum-tokens.json"
	defaultDatumInterval      = 30 * time.Second
	// socketMode is 0660 because on the local socket the peer's credentials are
	// the authentication: the boat service group may talk to the daemon and
	// nobody else may.
	socketMode = 0o660
	// shutdownGrace bounds the drain, and sits under the unit's
	// TimeoutStopSec=15 so the store still closes cleanly before systemd gives
	// up and sends SIGKILL. A verb that outlasts it keeps its claim in the
	// journal, so the restarted daemon can still tell what was in flight.
	shutdownGrace      = 10 * time.Second
	socketProbeTimeout = 200 * time.Millisecond
)

type daemonOptions struct {
	listenAddress string
	socketPath    string
	storePath     string
	tokenFilePath string
	serverName    string
	updateKeyPath string

	datumURL           string
	datumTokenFilePath string
	datumInterval      time.Duration
}

func daemon(arguments []string, errorOutput io.Writer) int {
	options, err := parseDaemonOptions(arguments, errorOutput)
	if err != nil {
		return exitUsage
	}
	tokens, err := token.Open(options.tokenFilePath)
	if err != nil {
		return reportError(errorOutput, err)
	}
	if err := options.requireTunnelToken(tokens); err != nil {
		return reportError(errorOutput, err)
	}
	if err := serve(options, tokens); err != nil {
		return reportError(errorOutput, err)
	}
	return exitSuccess
}

func parseDaemonOptions(arguments []string, errorOutput io.Writer) (daemonOptions, error) {
	var options daemonOptions
	flags := flag.NewFlagSet("boat daemon", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	flags.StringVar(&options.listenAddress, "listen", "", "address of the mgmt-tunnel listener; empty serves the socket only")
	flags.StringVar(&options.socketPath, "socket", defaultSocketPath, "path of the local control socket")
	flags.StringVar(&options.storePath, "store", defaultStorePath, "path of the observed-state and journal database")
	flags.StringVar(&options.tokenFilePath, "token-file", defaultTokenFilePath, "file holding the bearer token the tunnel listener demands")
	flags.StringVar(&options.serverName, "server-name", "", "this host's Frappe Server name; enables the §11.1 placement boot gate (empty leaves it inert)")
	flags.StringVar(&options.updateKeyPath, "update-key-file", defaultUpdateKeyPath, "file holding the ed25519 public key trusted to sign self-updates; absent disables POST /v1/update")
	flags.StringVar(&options.datumURL, "datum-url", "", "base URL of the datum ingest service; empty disables metrics export")
	flags.StringVar(&options.datumTokenFilePath, "datum-token-file", defaultDatumTokenFilePath, "file holding the datum bearer tokens (host + per-VM) Atlas ships")
	flags.DurationVar(&options.datumInterval, "datum-interval", defaultDatumInterval, "how often to collect and push metrics to datum")
	return options, flags.Parse(arguments)
}

// requireTunnelToken refuses a TCP listener with no token to demand — an open
// door onto the host, refused loudly instead of served. A socket-only daemon (no
// listen address) needs none: the unix socket's peer credentials are its
// authentication, so a host Atlas has not handed a token yet still serves its
// socket. Expiry counts as no token here too: a listener whose only token has
// already expired is the same open door, and is refused the same way.
func (options daemonOptions) requireTunnelToken(tokens *token.Store) error {
	if options.listenAddress != "" && tokens.Current() == "" {
		return fmt.Errorf("refusing to serve %s: no bearer token in %s", options.listenAddress, options.tokenFilePath)
	}
	return nil
}

// listening pairs a server with the listener it answers on, so the socket and
// the tunnel are shut down the same way.
type listening struct {
	server   *http.Server
	listener net.Listener
}

// serve is the whole daemon: build it, learn what the host already holds, run
// the loops that drive it, answer requests until the service manager asks for a
// stop, and put it all down in the order the pieces depend on each other.
func serve(options daemonOptions, tokens *token.Store) error {
	parts, err := build(options)
	if err != nil {
		return err
	}
	// Adoption runs inside startUp, before a listener exists to accept on.
	active, err := parts.startUp(context.Background(), options, tokens)
	if err != nil {
		return errors.Join(err, parts.close())
	}
	work := parts.runBackground()
	// A SIGHUP reloads the bearer token, so Atlas rotates it by replacing the file
	// and signalling this daemon rather than restarting it — no dropped tunnel and
	// no verb killed mid-flight to change a secret. It stops when serve returns.
	reloadCtx, stopReload := context.WithCancel(context.Background())
	defer stopReload()
	go reloadOnSignal(reloadCtx, tokens, parts.datumTokens)
	slog.Info("boat is serving", "socket", options.socketPath, "listen", options.listenAddress, "version", version.Version)
	served := serveUntilSignal(active)
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	return errors.Join(served, shutdown(ctx, active, work, parts, options.socketPath))
}

// openListeners binds the socket always and the tunnel only when asked. The two
// authenticate differently, so they get different handlers off the one server.
func openListeners(options daemonOptions, server *api.Server, tokens *token.Store) ([]listening, error) {
	socket, err := listenOnSocket(options.socketPath)
	if err != nil {
		return nil, err
	}
	active := []listening{{server: &http.Server{Handler: server.SocketHandler()}, listener: socket}}
	if options.listenAddress == "" {
		return active, nil
	}
	tunnel, err := net.Listen("tcp", options.listenAddress)
	if err != nil {
		socket.Close()
		return nil, fmt.Errorf("could not listen on %s: %w", options.listenAddress, err)
	}
	return append(active, listening{server: &http.Server{Handler: server.TunnelHandler(tokens.Current)}, listener: tunnel}), nil
}

// listenOnSocket binds the local control socket at 0660.
func listenOnSocket(path string) (net.Listener, error) {
	if err := checkSocketPathLength(path); err != nil {
		return nil, err
	}
	// 0750, matching the unit's RuntimeDirectoryMode: the boat group reaches the
	// socket inside, nobody else reaches the directory at all. systemd normally
	// creates it; this is for a daemon run by hand.
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("could not create the directory for %s: %w", path, err)
	}
	if err := clearStaleSocket(path); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("could not listen on %s: %w", path, err)
	}
	// The umask would otherwise decide who may talk to the daemon.
	if err := os.Chmod(path, socketMode); err != nil {
		listener.Close()
		return nil, fmt.Errorf("could not set the mode of %s: %w", path, err)
	}
	return listener, nil
}

// checkSocketPathLength refuses an over-long socket path before bind(2) does.
//
// AF_UNIX truncates at sun_path, and the kernel reports the overrun as a bare
// EINVAL — "bind: invalid argument", which names neither the limit nor the
// path as the cause. This is the same limit that forces the cd-and-relative-name
// dance around the jailed Firecracker socket, so it is a limit this project
// already knows it lives under; the only thing missing was saying so out loud.
func checkSocketPathLength(path string) error {
	if len(path) < paths.SunPathMax {
		return nil
	}
	return fmt.Errorf(
		"the socket path %s is %d bytes, and AF_UNIX allows at most %d — choose a shorter --socket",
		path, len(path), paths.SunPathMax-1,
	)
}

// clearStaleSocket removes a socket file a crash left behind, which would
// otherwise block the bind. It dials first: if something answers, another
// daemon already owns this host and a second one would race it for the VMs.
func clearStaleSocket(path string) error {
	if connection, err := net.DialTimeout("unix", path, socketProbeTimeout); err == nil {
		connection.Close()
		return fmt.Errorf("another boat daemon is already listening on %s", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("could not remove the stale socket %s: %w", path, err)
	}
	return nil
}

// reloadOnSignal reloads the bearer token on SIGHUP and returns when ctx is
// cancelled. It is how a rotation reaches a running daemon: `systemctl reload
// boat` (ExecReload → SIGHUP) after Atlas has replaced the token file, so the
// listener demands the new secret without the daemon restarting under its own
// tunnel.
//
// A failed reload is logged and the old token kept, deliberately: a half-written
// file or a bad rotation must not disarm the listener, only be ignored until a
// good one lands. SIGHUP's default action is to terminate, so registering this
// handler is also what keeps a reload from killing the daemon.
func reloadOnSignal(ctx context.Context, tokens *token.Store, datumTokens *datum.TokenSet) {
	hangups := make(chan os.Signal, 1)
	signal.Notify(hangups, syscall.SIGHUP)
	defer signal.Stop(hangups)
	for {
		select {
		case <-ctx.Done():
			return
		case <-hangups:
			if err := tokens.Reload(); err != nil {
				slog.Error("could not reload the bearer token on SIGHUP", "error", err)
				continue
			}
			slog.Info("reloaded the bearer token on SIGHUP")
			if datumTokens != nil {
				if err := datumTokens.Reload(); err != nil {
					slog.Error("could not reload the datum tokens on SIGHUP", "error", err)
				} else {
					slog.Info("reloaded the datum tokens on SIGHUP")
				}
			}
		}
	}
}

// serveUntilSignal returns when the service manager asks for a stop or a
// listener gives up. systemd sends SIGTERM; an operator running it by hand
// sends SIGINT.
func serveUntilSignal(active []listening) error {
	failures := make(chan error, len(active))
	for _, each := range active {
		go func() { failures <- each.server.Serve(each.listener) }()
	}
	notified, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-notified.Done():
		return nil
	case err := <-failures:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// shutdown puts the daemon down in the order its pieces depend on each other:
// the listeners drain first, so no new verb starts and every verb in flight
// finishes; then the loops that drive the host; and only then the files both of
// them were writing into.
//
// The store is closed ONLY when that quiesce actually completed. A verb that
// outlasted the drain still holds a claimed operation, and closing the store
// under it fails its CompleteOperation: the record stays non-terminal, the
// verb's own answer is lost, and the next start has to close the record on its
// behalf as a Failure (internal/reconcile, conclude) rather than with the
// outcome the verb actually reached. That is reachable on the ordinary
// binary-swap path — systemd stops the old daemon while a stop verb is inside
// its guest's 30-second drain — so the file is left open instead and the
// process exits with it open. bbolt commits every transaction to disk as it
// goes, so an unclosed file is a file the next start opens unharmed.
// ctx is the grace, and it is the caller's rather than this function's so that
// the branch below — the one that decides whether the store may be closed — is
// reachable from a test without a ten-second wait in it.
func shutdown(ctx context.Context, active []listening, work *background, parts *daemonParts, socketPath string) error {
	// Three things have to go quiet, and the operations are the one that is not
	// a request: a verb outlives the request that asked for it, so draining the
	// listeners says nothing about whether a start is still mid-flight. Without
	// this the store could close under a verb that had not yet recorded its
	// outcome — the one thing the journal exists to prevent.
	quiet := errors.Join(drain(ctx, active), parts.api.DrainOperations(ctx), work.stopAndWait(ctx))
	parts.api.StopBackground()
	if quiet != nil {
		slog.Error("boat is exiting with work still in flight, and is leaving its store open so that work can still record its outcome", "error", quiet)
		return errors.Join(quiet, removeSocket(socketPath))
	}
	return errors.Join(parts.close(), removeSocket(socketPath))
}

// drain stops both listeners accepting and waits for the requests already
// inside them. A Shutdown that returns ctx.Err() is a verb that outlasted the
// grace, which is the one thing the caller has to know.
func drain(ctx context.Context, active []listening) error {
	failures := make([]error, 0, len(active))
	for _, each := range active {
		failures = append(failures, each.server.Shutdown(ctx))
	}
	return errors.Join(failures...)
}

// removeSocket unlinks the control socket once nothing can arrive on it. Go
// unlinks it when its listener closes; it is removed anyway, because a file left
// behind is what blocks the next start.
func removeSocket(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
