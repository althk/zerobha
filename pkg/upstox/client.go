// Package upstox is a thin, read-only client for the Upstox market research
// endpoints the trading system consults before taking a position: instrument
// news and company fundamentals.
//
// It is deliberately transport-only — decoding and error handling live here,
// the policy that turns these payloads into a trade/no-trade decision lives in
// gate.go. Nothing in this package places orders; execution is Zerodha's.
//
// Authentication is a daily Upstox OAuth access token supplied by the caller.
// This package does not run the login flow.
package upstox

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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

// NewClient builds a client with a bounded per-request timeout. The gate runs
// on the candle hot path, so an unbounded call would stall the trading loop.
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

// NewsItem is one article from the news endpoint.
type NewsItem struct {
	Heading     string `json:"heading"`
	Summary     string `json:"summary"`
	Thumbnail   string `json:"thumbnail"`
	ArticleLink string `json:"article_link"`
	// PublishedTime is Unix milliseconds, as the API returns it.
	PublishedTime int64 `json:"published_time"`
}

// PublishedAt converts PublishedTime to a time.Time.
func (n NewsItem) PublishedAt() time.Time {
	return time.UnixMilli(n.PublishedTime)
}

type newsResponse struct {
	Status string                `json:"status"`
	Data   map[string][]NewsItem `json:"data"`
}

// InstrumentKey builds the NSE equity instrument key Upstox identifies a
// symbol by. Upstox keys cash-segment equities by ISIN, not trading symbol.
func InstrumentKey(isin string) string { return "NSE_EQ|" + isin }

// News fetches recent articles for one NSE equity, newest first as returned by
// the API. GET /v2/news?category=instrument_keys&instrument_keys=NSE_EQ|<isin>
func (c *Client) News(isin string, pageSize int) ([]NewsItem, error) {
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 25
	}
	key := InstrumentKey(isin)
	q := url.Values{}
	q.Set("category", "instrument_keys")
	q.Set("instrument_keys", key)
	q.Set("page_number", "1")
	q.Set("page_size", fmt.Sprintf("%d", pageSize))

	var out newsResponse
	if err := c.get("/news?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	// The payload is keyed by instrument key. Match ours, but fall back to the
	// sole entry if the API ever normalises the key differently.
	if items, ok := out.Data[key]; ok {
		return items, nil
	}
	if len(out.Data) == 1 {
		for _, items := range out.Data {
			return items, nil
		}
	}
	return nil, nil
}

// IncomeHistory is one reported period within a category.
type IncomeHistory struct {
	Value  float64 `json:"value"`
	Period string  `json:"period"`
	Change string  `json:"change"`
}

// IncomeCategory groups a headline line item (Revenue, Operating profit, Net
// profit) with its reported history, newest period first.
type IncomeCategory struct {
	Category string          `json:"category"`
	History  []IncomeHistory `json:"history"`
}

// IncomeStatement is the fundamentals payload for one company.
type IncomeStatement struct {
	Type            string           `json:"type"`
	TimePeriod      string           `json:"time_period"`
	UnitsIn         string           `json:"units_in"`
	IncomeStatement []IncomeCategory `json:"income_statement"`
}

type incomeResponse struct {
	Status string          `json:"status"`
	Data   IncomeStatement `json:"data"`
}

// Category returns the history for a headline category, matched
// case-insensitively on a substring (the API labels vary between
// "Net profit" and "Net Profit +").
func (s IncomeStatement) Category(name string) []IncomeHistory {
	want := strings.ToLower(name)
	for _, c := range s.IncomeStatement {
		if strings.Contains(strings.ToLower(c.Category), want) {
			return c.History
		}
	}
	return nil
}

// IncomeStatementQuarterly fetches the consolidated quarterly income statement
// for an ISIN. GET /v2/fundamentals/{isin}/income-statement
func (c *Client) IncomeStatementQuarterly(isin string) (IncomeStatement, error) {
	q := url.Values{}
	q.Set("type", "consolidated")
	q.Set("time_period", "quarterly")

	var out incomeResponse
	if err := c.get("/fundamentals/"+url.PathEscape(isin)+"/income-statement?"+q.Encode(), &out); err != nil {
		return IncomeStatement{}, err
	}
	return out.Data, nil
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
