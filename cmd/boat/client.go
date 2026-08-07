package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/frappe/boat/internal/wire"
)

// defaultSocketPath matches systemd/boat.service, which passes exactly this to
// `boat daemon`. It is /run/boat/boat.sock rather than the shorter
// /run/boat.sock the WO-0 contract names because /run is root-owned 0755 and
// the daemon runs non-root: systemd's RuntimeDirectory=boat gives it a
// directory it owns, and the socket lives inside that. The CLI's default has to
// match the unit's, or the operator's break-glass tool cannot reach the daemon
// its own host is running.
const defaultSocketPath = "/run/boat/boat.sock"

// basePath is the base the IDL's server URL declares (http://boat/v1). The
// daemon answers the bare paths too; the client uses the documented form.
const basePath = "/v1"

// socketPath honours BOAT_SOCKET so an operator whose daemon runs on a
// non-default socket — and a test with no /run to write to — reaches it through
// the same API rather than through a second path into the host.
func socketPath() string {
	if path := os.Getenv("BOAT_SOCKET"); path != "" {
		return path
	}
	return defaultSocketPath
}

// daemonClient speaks the documented API over the local socket. Every CLI verb
// below is one request from api/openapi.yaml and nothing else.
type daemonClient struct {
	transport *http.Client
}

func newDaemonClient(socketPath string) *daemonClient {
	dial := func(ctx context.Context, _ string, _ string) (net.Conn, error) {
		return new(net.Dialer).DialContext(ctx, "unix", socketPath)
	}
	// No client timeout: a start or a stop runs inline on the host and a cold
	// boot legitimately outlasts any deadline worth guessing at. The operator
	// interrupts; the daemon's journal keeps the result either way.
	return &daemonClient{transport: &http.Client{Transport: &http.Transport{DialContext: dial}}}
}

func (client *daemonClient) get(path string, into any) error {
	// The host in the URL is never resolved — the dialer always lands on the
	// socket — but net/http insists on one.
	request, err := http.NewRequest(http.MethodGet, "http://boat"+basePath+path, nil)
	if err != nil {
		return err
	}
	return client.do(request, into)
}

func (client *daemonClient) post(path string, body any, into any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPost, "http://boat"+basePath+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	return client.do(request, into)
}

func (client *daemonClient) put(path string, body any, into any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPut, "http://boat"+basePath+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	return client.do(request, into)
}

func (client *daemonClient) do(request *http.Request, into any) error {
	response, err := client.transport.Do(request)
	if err != nil {
		return fmt.Errorf("could not reach the boat daemon: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= http.StatusBadRequest {
		return errors.New(errorSentence(response))
	}
	return json.NewDecoder(response.Body).Decode(into)
}

// errorSentence reports the daemon's own sentence. A response that is not the
// documented Error shape has none, so its status stands in rather than a
// message the CLI invented.
func errorSentence(response *http.Response) string {
	var failure wire.Error
	if err := json.NewDecoder(response.Body).Decode(&failure); err == nil && failure.Error != "" {
		return failure.Error
	}
	return "the daemon answered " + response.Status
}
