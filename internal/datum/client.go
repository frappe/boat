// Package datum pushes host and per-VM metrics to a datum ingest endpoint. It is
// deliberately thin and best-effort: a push is one JSON POST under one bearer
// token (one resource_id), with a short timeout and no retry, because a lost
// metric is a gap in a chart and blocking the daemon to retry is worse.
package datum

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxBatch is datum's per-request sample cap; Push chunks to stay under it.
const maxBatch = 10000

// pushTimeout bounds a single ingest POST.
const pushTimeout = 2 * time.Second

// Sample is one datum sample. Labels is omitted from JSON when empty.
type Sample struct {
	Metric string            `json:"metric"`
	Value  float64           `json:"value"`
	TS     string            `json:"ts"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Client posts samples to a datum /v1/ingest endpoint.
type Client struct {
	ingestURL string
	http      *http.Client
}

// New builds a client for the datum base URL (e.g. "http://datum:8000" or
// "http://datum:8000/v1"; a trailing "/v1" or "/" is tolerated).
func New(baseURL string) *Client {
	base := strings.TrimRight(baseURL, "/")
	base = strings.TrimSuffix(base, "/v1")
	return &Client{
		ingestURL: base + "/v1/ingest",
		http:      &http.Client{Timeout: pushTimeout},
	}
}

// Push sends samples under one bearer token. It is fire-and-forget: an empty
// token, a marshal failure, a transport error, or a non-200 is returned as an
// error for the caller to log and discard. Samples are chunked to maxBatch. The
// return value is the total accepted count summed over the chunks.
func (c *Client) Push(ctx context.Context, token string, samples []Sample) (int, error) {
	if token == "" {
		return 0, fmt.Errorf("datum: empty token, skipping %d samples", len(samples))
	}
	accepted := 0
	for start := 0; start < len(samples); start += maxBatch {
		end := start + maxBatch
		if end > len(samples) {
			end = len(samples)
		}
		n, err := c.postChunk(ctx, token, samples[start:end])
		accepted += n
		if err != nil {
			return accepted, err
		}
	}
	return accepted, nil
}

func (c *Client) postChunk(ctx context.Context, token string, chunk []Sample) (int, error) {
	body, err := json.Marshal(map[string][]Sample{"samples": chunk})
	if err != nil {
		return 0, fmt.Errorf("datum: marshal samples: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ingestURL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("datum: build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.http.Do(request)
	if err != nil {
		return 0, fmt.Errorf("datum: post: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(response.Body, 256))
		return 0, fmt.Errorf("datum: ingest status %d: %s", response.StatusCode, strings.TrimSpace(string(snippet)))
	}
	var result struct {
		Accepted int `json:"accepted"`
	}
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	_ = json.Unmarshal(payload, &result)
	return result.Accepted, nil
}
