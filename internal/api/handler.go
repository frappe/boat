package api

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"github.com/frappe/boat/internal/wire"
)

// versionPrefix is the base path api/openapi.yaml declares in its server URL
// (http://boat/v1). The generated router registers the bare paths, so the
// daemon answers both forms: a caller that pastes the documented server URL and
// a caller that curls the socket by hand must not get different answers.
const versionPrefix = "/v1"

const bearerPrefix = "Bearer "

// TunnelHandler serves the mgmt-tunnel listener, where the bearer token Atlas
// minted for this host is the authentication. /health is exempt so a supervisor
// can probe a Boat that has not been handed a token yet.
func (server *Server) TunnelHandler(token string) http.Handler {
	return requireBearerToken(token, server.routes())
}

// SocketHandler serves /run/boat.sock, where unix peer credentials are the
// authentication: the socket is 0660 and group-owned by the boat service user,
// so anything that can connect is already authorised. Asking for a token here
// would only mean the operator's break-glass tool needs a secret to do what its
// group membership already permits.
func (server *Server) SocketHandler() http.Handler {
	return server.routes()
}

// routes mounts the generated router twice — bare and under /v1 — behind a JSON
// catch-all, so an undocumented path is refused in the shape the contract
// describes rather than in net/http's plain text.
func (server *Server) routes() http.Handler {
	operations := http.NewServeMux()
	operations.HandleFunc("/", pathNotFound)
	documented := wire.HandlerFromMux(server.strictHandler(), operations)

	router := http.NewServeMux()
	router.Handle("/", documented)
	router.Handle(versionPrefix+"/", http.StripPrefix(versionPrefix, documented))
	return router
}

// strictHandler wires the generated glue to error handlers that speak the
// contract's Error shape. Without them a malformed body would answer in plain
// text, and a client written against the IDL would have nothing to parse.
func (server *Server) strictHandler() wire.ServerInterface {
	return wire.NewStrictHandlerWithOptions(server, nil, wire.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  requestNotUnderstood,
		ResponseErrorHandlerFunc: responseNotWritten,
	})
}

// requireBearerToken guards the tunnel listener.
func requireBearerToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if isHealth(request.URL.Path) || tokenMatches(token, request.Header.Get("Authorization")) {
			next.ServeHTTP(writer, request)
			return
		}
		writeError(writer, http.StatusUnauthorized, "This request carried no valid bearer token.")
	})
}

// tokenMatches compares in constant time, so a caller cannot learn the token a
// byte at a time from how long the comparison took. An unset token matches
// nothing: a daemon holding no token must refuse the tunnel, not admit everyone.
func tokenMatches(token string, authorization string) bool {
	presented, found := strings.CutPrefix(authorization, bearerPrefix)
	if !found || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}

func isHealth(path string) bool {
	return path == "/health" || path == versionPrefix+"/health"
}

func pathNotFound(writer http.ResponseWriter, request *http.Request) {
	writeError(writer, http.StatusNotFound, "This host serves no operation at "+request.URL.Path+".")
}

// requestNotUnderstood answers a body the generated decoder could not read. The
// decoder's own message names Go types the caller cannot act on, so it is
// logged here and not returned.
func requestNotUnderstood(writer http.ResponseWriter, request *http.Request, err error) {
	slog.Warn("could not decode a request body", "path", request.URL.Path, "error", err)
	writeError(writer, http.StatusBadRequest, "This request body is not the JSON the operation expects.")
}

// responseNotWritten is the last resort: the handler already chose a response
// and writing it failed. Say so in one sentence and keep the detail on the host.
func responseNotWritten(writer http.ResponseWriter, request *http.Request, err error) {
	slog.Error("could not write a response", "path", request.URL.Path, "error", err)
	writeError(writer, http.StatusInternalServerError, "This host could not complete the request.")
}
