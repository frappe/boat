package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/token"
)

func TestDaemonOptionsDefaultToTheHostsLayout(t *testing.T) {
	options, err := parseDaemonOptions(nil, &bytes.Buffer{})

	if err != nil {
		t.Fatalf("could not parse an empty command line: %v", err)
	}
	want := daemonOptions{
		listenAddress: "",
		socketPath:    defaultSocketPath,
		storePath:     defaultStorePath,
		tokenFilePath: defaultTokenFilePath,
		serverName:    "",
		updateKeyPath: defaultUpdateKeyPath,
	}
	if options != want {
		t.Errorf("got %+v, want %+v", options, want)
	}
}

func TestDaemonOptionsTakeTheirOverrides(t *testing.T) {
	arguments := []string{"--listen", "10.0.0.1:8080", "--socket", "/tmp/boat.sock", "--store", "/tmp/boat.db", "--token-file", "/tmp/token"}

	options, err := parseDaemonOptions(arguments, &bytes.Buffer{})

	if err != nil {
		t.Fatalf("could not parse %v: %v", arguments, err)
	}
	if options.listenAddress != "10.0.0.1:8080" || options.socketPath != "/tmp/boat.sock" {
		t.Errorf("got %+v, want the given listen address and socket", options)
	}
	if options.storePath != "/tmp/boat.db" || options.tokenFilePath != "/tmp/token" {
		t.Errorf("got %+v, want the given store and token file", options)
	}
}

// A TCP listener with no token would be an open door onto the host: better to
// fail to start than to start wrong. (Token parsing and expiry are the token
// package's own tests; this asserts only the daemon's start-time gate.)
func TestATunnelListenerWithoutATokenIsRefused(t *testing.T) {
	options := daemonOptions{listenAddress: "127.0.0.1:0", tokenFilePath: filepath.Join(t.TempDir(), "absent")}
	tokens, err := token.Open(options.tokenFilePath)
	if err != nil {
		t.Fatalf("open token store: %v", err)
	}

	err = options.requireTunnelToken(tokens)

	if err == nil {
		t.Fatal("a tunnel listener was accepted with no token")
	}
	if !strings.Contains(err.Error(), "refusing") {
		t.Errorf("got %q, want a refusal that says so", err)
	}
}

// A host Atlas has not handed a token yet still serves its local socket.
func TestASocketOnlyDaemonNeedsNoToken(t *testing.T) {
	options := daemonOptions{tokenFilePath: filepath.Join(t.TempDir(), "absent")}
	tokens, err := token.Open(options.tokenFilePath)
	if err != nil {
		t.Fatalf("open token store: %v", err)
	}

	if err := options.requireTunnelToken(tokens); err != nil {
		t.Errorf("a socket-only daemon was refused: %v", err)
	}
	if got := tokens.Current(); got != "" {
		t.Errorf("got token %q, want none for a host with no token file", got)
	}
}

// The socket's mode is its access control, so the umask must not get a say.
func TestTheControlSocketIsGroupWritableAndNoMore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boat.sock")

	listener, err := listenOnSocket(path)
	if err != nil {
		t.Fatalf("could not listen on %s: %v", path, err)
	}
	defer listener.Close()

	information, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the socket was not created: %v", err)
	}
	if information.Mode().Perm() != socketMode {
		t.Errorf("got mode %o, want %o", information.Mode().Perm(), socketMode)
	}
}

// An over-long path must be refused by name. The kernel's own answer is a bare
// EINVAL, which sends the reader looking for a permissions or a syntax problem
// rather than at the length.
func TestAnOverLongSocketPathIsRefusedWithItsLength(t *testing.T) {
	path := "/tmp/" + strings.Repeat("x", 120) + ".sock"

	_, err := listenOnSocket(path)

	if err == nil {
		t.Fatal("an over-long socket path was accepted")
	}
	for _, want := range []string{"130 bytes", "107", "shorter"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

func TestAStaleSocketIsClearedButALiveOneIsNot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boat.sock")
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("could not listen on %s: %v", path, err)
	}

	if _, err := listenOnSocket(path); err == nil {
		t.Error("a second daemon bound a socket another one was answering on")
	}

	// Closing the listener without unlinking is what a crash leaves behind.
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	listener, err := listenOnSocket(path)
	if err != nil {
		t.Fatalf("a stale socket was not cleared: %v", err)
	}
	listener.Close()
}

// The store outlives the verbs that write into it. Closing it under a verb
// still in flight fails that verb's CompleteOperation, and the operation stays
// non-terminal forever: the Atlas Task behind it never completes, and the retry
// reads the same Running record and waits again.
func TestShutdownWaitsForAVerbInFlightBeforeClosingTheStore(t *testing.T) {
	parts := newTestParts(t, &fakeMechanics{})
	verb, socketPath := serveOneBlockingVerb(t, parts)

	<-verb.entered
	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		stopped <- shutdown(ctx, verb.active, newBackground(), parts, socketPath)
	}()
	// Long enough for the drain to be under way, so that the record below is
	// written by a verb the shutdown is already waiting for.
	time.Sleep(50 * time.Millisecond)
	close(verb.release)

	if err := <-verb.recorded; err != nil {
		t.Fatalf("the store was closed under a verb still in flight: %v", err)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("the shutdown failed: %v", err)
	}
	if err := parts.store.PutVirtualMachine(model.VirtualMachine{UUID: "after"}); err == nil {
		t.Error("the store was left open after a clean shutdown")
	}
}

// A verb that outlasts the grace keeps the store: the daemon exits with the
// file open rather than stranding that operation's record non-terminal. bbolt
// commits as it goes, so the next start opens it unharmed.
func TestAVerbThatOutlastsTheGraceKeepsTheStoreOpen(t *testing.T) {
	parts := newTestParts(t, &fakeMechanics{})
	verb, socketPath := serveOneBlockingVerb(t, parts)

	<-verb.entered
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	err := shutdown(expired, verb.active, newBackground(), parts, socketPath)

	if err == nil {
		t.Error("a shutdown that abandoned a verb in flight reported success")
	}
	if putErr := parts.store.PutVirtualMachine(model.VirtualMachine{UUID: "still-writable"}); putErr != nil {
		t.Errorf("the store was closed under a verb the shutdown could not wait for: %v", putErr)
	}
	close(verb.release)
	if recordErr := <-verb.recorded; recordErr != nil {
		t.Errorf("the abandoned verb could not record its outcome: %v", recordErr)
	}
}

// blockingVerb is one request held inside its handler, which is what a stop verb
// inside its guest's 30-second drain looks like to a shutdown.
type blockingVerb struct {
	active   []listening
	entered  chan struct{}
	release  chan struct{}
	recorded chan error
}

// serveOneBlockingVerb starts a listener whose handler writes to the store after
// the test lets it go, and puts a request inside that handler.
func serveOneBlockingVerb(t *testing.T, parts *daemonParts) (*blockingVerb, string) {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "boat.sock")
	listener, err := listenOnSocket(socketPath)
	if err != nil {
		t.Fatalf("could not listen on %s: %v", socketPath, err)
	}
	verb := &blockingVerb{
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		recorded: make(chan error, 1),
	}
	verb.active = []listening{{server: &http.Server{Handler: verb.handler(parts)}, listener: listener}}
	go verb.active[0].server.Serve(listener)
	verb.request(t, socketPath)
	return verb, socketPath
}

func (verb *blockingVerb) handler(parts *daemonParts) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(verb.entered)
		<-verb.release
		// The last thing every verb does: write its terminal record. This is the
		// write that a store closed too early loses.
		verb.recorded <- parts.store.PutVirtualMachine(model.VirtualMachine{UUID: "in-flight"})
		writer.WriteHeader(http.StatusOK)
	})
}

// request puts one request into the handler and leaves it there. The connection
// is closed when the test ends, not when the response arrives: a client that
// hung up would let the drain finish early and prove nothing.
func (verb *blockingVerb) request(t *testing.T, socketPath string) {
	t.Helper()
	connection, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("could not dial %s: %v", socketPath, err)
	}
	t.Cleanup(func() { connection.Close() })
	if _, err := fmt.Fprint(connection, "POST /vms/x/stop HTTP/1.1\r\nHost: boat\r\nContent-Length: 0\r\n\r\n"); err != nil {
		t.Fatalf("could not send the request: %v", err)
	}
}
