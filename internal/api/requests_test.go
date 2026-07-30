package api

// The request helpers every handler test speaks through: the surface is HTTP, so
// the tests exercise it as HTTP rather than by calling handler methods that
// would skip the router, the decoder and the error handlers.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/wire"
)

func postJSON(t *testing.T, handler http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("could not encode the request body: %v", err)
	}
	return postBody(handler, path, string(encoded))
}

func postBody(handler http.Handler, path string, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func putJSON(t *testing.T, handler http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return putJSONMatching(t, handler, path, body, "")
}

// putJSONMatching sends the CAS precondition of §11.2 as a real header, because
// that is where it lives: a test that handed the epoch to a handler directly
// would prove nothing about whether the generated router binds it.
func putJSONMatching(
	t *testing.T, handler http.Handler, path string, body any, ifMatch string,
) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("could not encode the request body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPut, path, strings.NewReader(string(encoded)))
	if ifMatch != "" {
		request.Header.Set("If-Match", ifMatch)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func get(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

func decodeOperation(t *testing.T, recorder *httptest.ResponseRecorder) wire.Operation {
	t.Helper()
	var operation wire.Operation
	decode(t, recorder, &operation)
	return operation
}

func decodeError(t *testing.T, recorder *httptest.ResponseRecorder) wire.Error {
	t.Helper()
	var failure wire.Error
	decode(t, recorder, &failure)
	return failure
}

func decode(t *testing.T, recorder *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), into); err != nil {
		t.Fatalf("could not decode %q: %v", recorder.Body.String(), err)
	}
}
