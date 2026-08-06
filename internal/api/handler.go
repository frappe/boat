package api

import (
	"context"
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
//
// It takes the token as a getter, not a value, because the token rotates under
// a running daemon: Atlas replaces the file and signals a reload, and the very
// next request must be judged against the new secret. current() is read once per
// request and returns empty for a token that has expired or been cleared, which
// tokenMatches then refuses.
func (server *Server) TunnelHandler(current func() string) http.Handler {
	return requireBearerToken(current, server.routes())
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
	// Both listeners, because whether a caller waits is the caller's business and
	// not the transport's: Atlas polls over the tunnel, an operator's break-glass
	// verb blocks on the socket, and either could want the other.
	return preferRespondAsync(router)
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

// requireBearerToken guards the tunnel listener. current is read per request, so
// a token rotated or expired between two requests is honoured on the second.
func requireBearerToken(current func() string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if isHealth(request.URL.Path) || tokenMatches(current(), request.Header.Get("Authorization")) {
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

// respondAsyncKey marks a request whose caller polls rather than waits.
type respondAsyncKey struct{}

// preferRespondAsync reads RFC 7240's `Prefer: respond-async`.
//
// A header rather than a field in every verb's request body, because it is not
// about the work: the same start does the same thing either way, and only the
// answer differs. Atlas sends it and then polls `GET /ops/{operation_id}`, so a
// verb that takes half an hour holds no connection and a dropped one loses no
// outcome. `boat vm start` sends nothing and blocks, which is what an operator
// at a terminal wants.
func preferRespondAsync(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(strings.ToLower(request.Header.Get("Prefer")), "respond-async") {
			request = request.WithContext(context.WithValue(request.Context(), respondAsyncKey{}, true))
		}
		next.ServeHTTP(writer, request)
	})
}

// respondAsync reports whether this request's caller asked to poll.
func respondAsync(ctx context.Context) bool {
	asked, _ := ctx.Value(respondAsyncKey{}).(bool)
	return asked
}
