package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const testToken = "a-short-lived-per-host-token"

func newTestTunnel(token string) http.Handler {
	return newTestServer(newFakeStore(), &fakeVirtualMachines{}).TunnelHandler(func() string { return token })
}

func TestTunnelRefusesAMissingOrWrongToken(t *testing.T) {
	tunnel := newTestTunnel(testToken)
	headers := map[string]string{
		"missing":    "",
		"wrong":      "Bearer not-the-token",
		"unprefixed": testToken,
	}

	for name, authorization := range headers {
		recorder := getWithToken(tunnel, "/vms", authorization)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s token: got %d, want 401", name, recorder.Code)
		}
		if decodeError(t, recorder).Error == "" {
			t.Errorf("%s token: the refusal carried no sentence", name)
		}
	}
}

func TestTunnelAdmitsTheRightToken(t *testing.T) {
	recorder := getWithToken(newTestTunnel(testToken), "/vms", "Bearer "+testToken)

	if recorder.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
}

// A supervisor has to be able to probe a Boat that has not been handed a token
// yet, which is why /health is the one exemption.
func TestHealthIsReachableWithoutAToken(t *testing.T) {
	for _, path := range []string{"/health", versionPrefix + "/health"} {
		recorder := getWithToken(newTestTunnel(testToken), path, "")
		if recorder.Code != http.StatusOK {
			t.Errorf("%s: got %d, want 200: %s", path, recorder.Code, recorder.Body)
		}
	}
}

// A daemon with no token must refuse the tunnel rather than admit everyone: an
// empty configured token is not a match for an empty presented one.
func TestATunnelWithoutATokenAdmitsNobody(t *testing.T) {
	tunnel := newTestTunnel("")

	for _, authorization := range []string{"", "Bearer "} {
		if recorder := getWithToken(tunnel, "/vms", authorization); recorder.Code != http.StatusUnauthorized {
			t.Errorf("%q: got %d, want 401", authorization, recorder.Code)
		}
	}
}

// On the socket the peer's credentials are the authentication, so no request
// there carries a token at all.
func TestSocketHandlerNeedsNoToken(t *testing.T) {
	socket := newTestServer(newFakeStore(), &fakeVirtualMachines{}).SocketHandler()

	for _, path := range []string{"/health", "/host", "/vms", "/export"} {
		if recorder := get(t, socket, path); recorder.Code != http.StatusOK {
			t.Errorf("%s: got %d, want 200: %s", path, recorder.Code, recorder.Body)
		}
	}
}

// api/openapi.yaml declares http://boat/v1 as the server URL while the
// generated router registers the bare paths; both have to answer or a client
// written from the IDL talks to nothing.
func TestTheDocumentedBasePathAlsoRoutes(t *testing.T) {
	socket := newTestServer(newFakeStore(), &fakeVirtualMachines{}).SocketHandler()

	if recorder := get(t, socket, versionPrefix+"/vms"); recorder.Code != http.StatusOK {
		t.Errorf("got %d, want 200: %s", recorder.Code, recorder.Body)
	}
}

func TestAnUndocumentedPathIsRefusedAsJson(t *testing.T) {
	socket := newTestServer(newFakeStore(), &fakeVirtualMachines{}).SocketHandler()

	recorder := get(t, socket, "/vms/one/evacuate")

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404: %s", recorder.Code, recorder.Body)
	}
	if decodeError(t, recorder).Error == "" {
		t.Error("the refusal carried no sentence")
	}
}

func getWithToken(handler http.Handler, path string, authorization string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
