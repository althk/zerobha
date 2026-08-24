package upstox

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

// HistoryBaseURL is the Upstox API v3 root. The historical-candle endpoints
// live on v3 while the research endpoints this package started with are v2, so
// the two roots coexist.
const HistoryBaseURL = "https://api.upstox.com/v3"

// istLocation is the exchange timezone. Upstox returns RFC3339 timestamps with
// a +05:30 offset already, so this is only used to bound requests by date.
var istLocation = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		return time.FixedZone("IST", 5*3600+1800)
	}
	return loc
}()

// candleResponse is the shape both the live and expired candle endpoints
// return: a positional array per bar, newest first.
//
//	[timestamp, open, high, low, close, volume, open_interest]
type candleResponse struct {
	Status string `json:"status"`
	Data   struct {
		Candles [][]any `json:"candles"`
	} `json:"data"`
}

// HistoricalCandles fetches OHLC bars for a currently-listed instrument.
//
//	unit     : "minutes", "hours", "days", "weeks", "months"
//	interval : the multiple of unit, e.g. 5 with "minutes" for 5-minute bars
//
// Index instrument keys are "NSE_INDEX|Nifty 50" and "BSE_INDEX|SENSEX".
//
// This endpoint serves index history without authentication, which is why the
// index leg of the options work needs no entitlement while the expired-contract
// endpoints below do. The token is still sent when configured.
func (c *Client) HistoricalCandles(instrumentKey, unit string, interval int, from, to time.Time) ([]models.Candle, error) {
	path := fmt.Sprintf("/historical-candle/%s/%s/%d/%s/%s",
		url.PathEscape(instrumentKey), unit, interval,
		to.Format("2006-01-02"), from.Format("2006-01-02"))
	return c.candles(HistoryBaseURL, path, instrumentKey)
}

// ExpiredContract is one expired option contract as listed by Upstox.
type ExpiredContract struct {
	Name             string  `json:"name"`
	Segment          string  `json:"segment"`
	Exchange         string  `json:"exchange"`
	Expiry           string  `json:"expiry"`
	InstrumentKey    string  `json:"instrument_key"`
	TradingSymbol    string  `json:"trading_symbol"`
	InstrumentType   string  `json:"instrument_type"` // CE or PE
	StrikePrice      float64 `json:"strike_price"`
	LotSize          int     `json:"lot_size"`
	TickSize         float64 `json:"tick_size"`
	UnderlyingKey    string  `json:"underlying_key"`
	UnderlyingSymbol string  `json:"underlying_symbol"`
	Weekly           bool    `json:"weekly"`
}

// ExpiredExpiries lists the expiry dates available for an underlying, newest
// last, in "2006-01-02" form.
//
// Upstox documents this as covering roughly the last six months, which bounds
// any option-level backtest built on it — well under the 250-session sample
// CLAUDE.md requires before a result means anything.
func (c *Client) ExpiredExpiries(underlyingKey string) ([]string, error) {
	var out struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	path := "/expired-instruments/expiries?instrument_key=" + url.QueryEscape(underlyingKey)
	if err := c.get(path, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// ExpiredOptionContracts lists the option contracts that expired on a given
// date for an underlying, with their strikes and lot sizes.
func (c *Client) ExpiredOptionContracts(underlyingKey, expiry string) ([]ExpiredContract, error) {
	var out struct {
		Status string            `json:"status"`
		Data   []ExpiredContract `json:"data"`
	}
	path := fmt.Sprintf("/expired-instruments/option/contract?instrument_key=%s&expiry_date=%s",
		url.QueryEscape(underlyingKey), url.QueryEscape(expiry))
	if err := c.get(path, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// ExpiredHistoricalCandles fetches bars for an already-expired contract, keyed
// by the "NSE_FO|47983|17-04-2025" form that ExpiredOptionContracts returns.
//
// Unlike HistoricalCandles this needs both a valid token and an Upstox Plus
// entitlement; without it the call fails with an authorisation error rather
// than returning an empty series, so the caller can tell the difference.
func (c *Client) ExpiredHistoricalCandles(expiredInstrumentKey, interval string, from, to time.Time) ([]models.Candle, error) {
	path := fmt.Sprintf("/expired-instruments/historical-candle/%s/%s/%s/%s",
		url.PathEscape(expiredInstrumentKey), interval,
		to.Format("2006-01-02"), from.Format("2006-01-02"))
	// The expired endpoints are v2.
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return c.candles(base, path, expiredInstrumentKey)
}

// candles performs the GET and converts the positional arrays into candles,
// oldest first — the order the backtester replays and every indicator assumes.
func (c *Client) candles(base, path, symbol string) ([]models.Candle, error) {
	var out candleResponse
	if err := c.getFrom(base, path, &out); err != nil {
		return nil, err
	}

	candles := make([]models.Candle, 0, len(out.Data.Candles))
	for i := len(out.Data.Candles) - 1; i >= 0; i-- {
		row := out.Data.Candles[i]
		if len(row) < 6 {
			continue
		}
		ts, ok := row[0].(string)
		if !ok {
			continue
		}
		start, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			continue
		}
		num := func(idx int) decimal.Decimal {
			f, ok := row[idx].(float64)
			if !ok {
				return decimal.Zero
			}
			return decimal.NewFromFloat(f)
		}
		candles = append(candles, models.Candle{
			Symbol:     symbol,
			Open:       num(1),
			High:       num(2),
			Low:        num(3),
			Close:      num(4),
			Volume:     num(5),
			StartTime:  start,
			IsComplete: true,
		})
	}
	return candles, nil
}

// getFrom is get() against an explicit base URL, and without the hard
// requirement of a token: the v3 historical endpoints serve index data
// anonymously, and refusing to call them for lack of a token would block the
// one part of this work that needs no entitlement.
func (c *Client) getFrom(base, path string, v any) error {
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return fmt.Errorf("upstox: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("upstox: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("upstox: read %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstox: %s: HTTP %d: %s", path, resp.StatusCode, truncate(strings.TrimSpace(string(body)), 300))
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("upstox: decode %s: %w", path, err)
	}
	return nil
}
