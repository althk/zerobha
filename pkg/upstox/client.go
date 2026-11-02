// Package upstox is a thin, read-only client for Upstox market data: index
// history and expired option contracts (history.go). Nothing in this package
// places orders; execution is Zerodha's.
//
// Authentication is a daily Upstox OAuth access token supplied by the caller.
// This package does not run the login flow.
package upstox

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultBaseURL is the Upstox API v2 root. Overridable for tests.
const DefaultBaseURL = "https://api.upstox.com/v2"

// Client calls the Upstox REST API with a bearer access token.
type Client struct {
	BaseURL     string
	AccessToken string
	HTTP        *http.Client
}

// NewClient builds a client with a bounded per-request timeout.
func NewClient(accessToken string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Client{
		BaseURL:     DefaultBaseURL,
		AccessToken: accessToken,
		HTTP:        &http.Client{Timeout: timeout},
	}
}

// get performs an authenticated GET and decodes the JSON body into v.
func (c *Client) get(path string, v any) error {
	if c.AccessToken == "" {
		return fmt.Errorf("upstox: no access token configured")
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}

	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return fmt.Errorf("upstox: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upstox: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("upstox: read %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		// Truncate: an HTML error page from a proxy would otherwise flood logs.
		snippet := string(body)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return fmt.Errorf("upstox: %s returned %d: %s", path, resp.StatusCode, snippet)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("upstox: decode %s: %w", path, err)
	}
	return nil
}

// truncate shortens s to n bytes for an error message.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
