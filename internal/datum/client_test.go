package datum

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPushPostsSamplesWithBearerToken(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string][]Sample
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"accepted":1}`))
	}))
	defer server.Close()

	client := New(server.URL)
	accepted, err := client.Push(context.Background(), "tok-123", []Sample{
		{Metric: "host_cpu_cores", Value: 8, TS: "2026-08-06T22:30:00Z"},
	})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if accepted != 1 {
		t.Errorf("accepted = %d, want 1 (from the accepted:1 response body)", accepted)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/ingest" {
		t.Errorf("path = %q, want /v1/ingest", gotPath)
	}
	if want := "Bearer tok-123"; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
	samples := gotBody["samples"]
	if len(samples) != 1 {
		t.Fatalf("got %d samples, want 1", len(samples))
	}
	s := samples[0]
	if s.Metric != "host_cpu_cores" || s.Value != 8 || s.TS != "2026-08-06T22:30:00Z" {
		t.Errorf("sample = %+v, want Metric=host_cpu_cores Value=8 TS=2026-08-06T22:30:00Z", s)
	}
}

func TestPushEmptyTokenSkips(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New(server.URL)
	_, err := client.Push(context.Background(), "", []Sample{{Metric: "m", Value: 1, TS: "t"}})
	if err == nil {
		t.Fatal("Push with empty token: got nil error, want non-nil")
	}
	if called {
		t.Error("handler was called; an empty token must skip the HTTP call")
	}
}

func TestPushNon200IsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"error":"bad sample"}`))
	}))
	defer server.Close()

	client := New(server.URL)
	_, err := client.Push(context.Background(), "tok", []Sample{{Metric: "m", Value: 1, TS: "t"}})
	if err == nil {
		t.Fatal("Push: got nil error, want non-nil")
	}
	if !strings.Contains(err.Error(), "422") {
		t.Errorf("error %q does not mention 422", err)
	}
}

func TestPushChunksAtMaxBatch(t *testing.T) {
	var posts int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&posts, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	samples := make([]Sample, maxBatch+1)
	for i := range samples {
		samples[i] = Sample{Metric: "m", Value: float64(i), TS: "t"}
	}
	client := New(server.URL)
	accepted, err := client.Push(context.Background(), "tok", samples)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if accepted != 0 {
		t.Errorf("accepted = %d, want 0 (server sends no accepted body)", accepted)
	}
	if got := atomic.LoadInt64(&posts); got != 2 {
		t.Errorf("got %d POSTs, want 2 (maxBatch=%d)", got, maxBatch)
	}
}
