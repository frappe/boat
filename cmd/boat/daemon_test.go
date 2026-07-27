package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
// fail to start than to start wrong.
func TestATunnelListenerWithoutATokenIsRefused(t *testing.T) {
	options := daemonOptions{listenAddress: "127.0.0.1:0", tokenFilePath: filepath.Join(t.TempDir(), "absent")}

	_, err := options.bearerToken()

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

	token, err := options.bearerToken()

	if err != nil || token != "" {
		t.Errorf("got %q, %v; want no token and no error", token, err)
	}
}

func TestTheTokenIsReadWithoutItsTrailingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("a-short-lived-token\n"), 0o600); err != nil {
		t.Fatalf("could not write the token: %v", err)
	}
	options := daemonOptions{listenAddress: "127.0.0.1:0", tokenFilePath: path}

	token, err := options.bearerToken()

	if err != nil {
		t.Fatalf("could not read the token: %v", err)
	}
	if token != "a-short-lived-token" {
		t.Errorf("got %q, want the token without its newline", token)
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
