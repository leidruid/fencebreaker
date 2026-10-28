// Package httpx is a small shared HTTP client for downloads (geodata, list
// subscriptions) with timeouts and size caps.
package httpx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client wraps an http.Client with sane timeouts.
type Client struct {
	HTTP    *http.Client
	MaxBody int64
}

// New creates a client with the given response body cap (bytes).
func New(maxBody int64) *Client {
	return &Client{
		HTTP: &http.Client{
			Timeout: 60 * time.Second,
		},
		MaxBody: maxBody,
	}
}

// Get fetches a URL and returns the capped body.
func (c *Client) Get(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "fencebreakerd/0.1")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("httpx: HTTP %d for %s", resp.StatusCode, url)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.MaxBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > c.MaxBody {
		return nil, fmt.Errorf("httpx: body over %d bytes for %s", c.MaxBody, url)
	}
	return data, nil
}
