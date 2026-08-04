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
	"strings"
	"syscall"
	"time"

	"github.com/frappe/boat/internal/api"
	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/version"
)

const (
	defaultStorePath     = "/var/lib/boat/boat.db"
	defaultTokenFilePath = "/etc/boat/token"
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
}

func daemon(arguments []string, errorOutput io.Writer) int {
	options, err := parseDaemonOptions(arguments, errorOutput)
	if err != nil {
		return exitUsage
	}
	token, err := options.bearerToken()
	if err != nil {
		return reportError(errorOutput, err)
	}
	if err := serve(options, token); err != nil {
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
	return options, flags.Parse(arguments)
}

// bearerToken reads the token the tunnel listener will demand. A TCP listener
// without one is an open door onto the host, so it is refused loudly instead of
// served.
func (options daemonOptions) bearerToken() (string, error) {
	token, err := readTokenFile(options.tokenFilePath)
	if err != nil {
		return "", err
	}
	if options.listenAddress != "" && token == "" {
		return "", fmt.Errorf("refusing to serve %s: no bearer token in %s", options.listenAddress, options.tokenFilePath)
	}
	return token, nil
}

// readTokenFile treats a missing file as no token: a socket-only daemon is
// legitimate on a host Atlas has not handed a token yet.
func readTokenFile(path string) (string, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("could not read the token file %s: %w", path, err)
	}
	return strings.TrimSpace(string(content)), nil
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
func serve(options daemonOptions, token string) error {
	parts, err := build(options)
	if err != nil {
		return err
	}
	// Adoption runs inside startUp, before a listener exists to accept on.
	active, err := parts.startUp(context.Background(), options, token)
	if err != nil {
		return errors.Join(err, parts.close())
	}
	work := parts.runBackground()
	slog.Info("boat is serving", "socket", options.socketPath, "listen", options.listenAddress, "version", version.Version)
	served := serveUntilSignal(active)
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	return errors.Join(served, shutdown(ctx, active, work, parts, options.socketPath))
}

// openListeners binds the socket always and the tunnel only when asked. The two
// authenticate differently, so they get different handlers off the one server.
func openListeners(options daemonOptions, server *api.Server, token string) ([]listening, error) {
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
	return append(active, listening{server: &http.Server{Handler: server.TunnelHandler(token)}, listener: tunnel}), nil
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
	quiet := errors.Join(drain(ctx, active), work.stopAndWait(ctx))
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
