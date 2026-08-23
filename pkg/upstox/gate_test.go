package upstox

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubServer serves canned news and income-statement payloads and records the
// requests it saw, so the tests can assert on the wire format as well as the
// policy applied to it.
type stubServer struct {
	news    string
	income  string
	newsErr int // non-zero HTTP status to return for /news
	paths   []string
}

func (s *stubServer) start(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.paths = append(s.paths, r.URL.String())
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("missing bearer token, got %q", got)
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/news"):
			if s.newsErr != 0 {
				w.WriteHeader(s.newsErr)
				_, _ = w.Write([]byte(`{"status":"error"}`))
				return
			}
			_, _ = w.Write([]byte(s.news))
		case strings.Contains(r.URL.Path, "income-statement"):
			_, _ = w.Write([]byte(s.income))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient("test-token", 2*time.Second)
	c.BaseURL = srv.URL
	return c
}

// newsJSON builds a payload with articles at the given offsets before asOf.
func newsJSON(isin string, items ...NewsItem) string {
	var parts []string
	for _, it := range items {
		parts = append(parts, fmt.Sprintf(
			`{"heading":%q,"summary":%q,"thumbnail":"","article_link":"","published_time":%d}`,
			it.Heading, it.Summary, it.PublishedTime))
	}
	return fmt.Sprintf(`{"status":"success","data":{"NSE_EQ|%s":[%s]},"metadata":{}}`,
		isin, strings.Join(parts, ","))
}

// incomeJSON builds a quarterly net-profit history, newest first.
func incomeJSON(period string, values ...float64) string {
	var parts []string
	for i, v := range values {
		parts = append(parts, fmt.Sprintf(`{"value":%g,"period":%q,"change":"0"}`, v, period))
		_ = i
	}
	return fmt.Sprintf(`{"status":"success","data":{"type":"consolidated","time_period":"quarterly","units_in":"crore","income_statement":[{"category":"Net profit","history":[%s]}]}}`,
		strings.Join(parts, ","))
}

type mapLookup map[string]string

func (m mapLookup) ISIN(symbol string) (string, bool) {
	isin, ok := m[NormalizeSymbol(symbol)]
	return isin, ok
}

const testISIN = "INE002A01018"

var asOf = time.Date(2026, 3, 3, 9, 35, 0, 0, time.UTC)

func hoursAgo(h int) int64 { return asOf.Add(-time.Duration(h) * time.Hour).UnixMilli() }

func newGateFor(t *testing.T, s *stubServer) *Gate {
	t.Helper()
	return NewGate(s.start(t), mapLookup{"TEST": testISIN}, GateConfig{
		NewsLookback:      48 * time.Hour,
		MaxProfitDropPct:  25,
		MaxResultsAgeDays: 180,
	})
}

func TestGateBlocksOnRecentBadNews(t *testing.T) {
	s := &stubServer{
		news: newsJSON(testISIN, NewsItem{
			Heading:       "SEBI opens probe into TEST accounting",
			PublishedTime: hoursAgo(6),
		}),
		income: incomeJSON("Dec 2025", 100, 95, 90, 85, 98),
	}
	v, err := newGateFor(t, s).Assess("TEST", asOf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Allow {
		t.Fatalf("a fraud probe must block the fade, got %+v", v)
	}
	if !strings.Contains(v.Reason, "sebi") && !strings.Contains(v.Reason, "probe") {
		t.Errorf("reason should name the matched keyword, got %q", v.Reason)
	}
}

func TestGateIgnoresStaleNews(t *testing.T) {
	s := &stubServer{
		// Same damaging headline, but from two weeks ago: it does not explain
		// today's gap.
		news: newsJSON(testISIN, NewsItem{
			Heading:       "SEBI opens probe into TEST accounting",
			PublishedTime: hoursAgo(24 * 14),
		}),
		income: incomeJSON("Dec 2025", 100, 95, 90, 85, 98),
	}
	v, err := newGateFor(t, s).Assess("TEST", asOf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Allow {
		t.Fatalf("news outside the lookback must not block, got %+v", v)
	}
}

func TestGateIgnoresNewsPublishedAfterTheDecision(t *testing.T) {
	s := &stubServer{
		news: newsJSON(testISIN, NewsItem{
			Heading:       "TEST plunges after fraud allegations",
			PublishedTime: asOf.Add(2 * time.Hour).UnixMilli(),
		}),
		income: incomeJSON("Dec 2025", 100, 95, 90, 85, 98),
	}
	v, err := newGateFor(t, s).Assess("TEST", asOf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Allow {
		t.Fatalf("an article published after the decision is not evidence about it, got %+v", v)
	}
}

func TestGateBlocksOnCollapsedQuarter(t *testing.T) {
	s := &stubServer{
		news: newsJSON(testISIN),
		// Latest 40 vs year-ago 100: a 60% YoY fall, past the 25% threshold.
		income: incomeJSON("Dec 2025", 40, 95, 90, 85, 100),
	}
	v, err := newGateFor(t, s).Assess("TEST", asOf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Allow {
		t.Fatalf("a 60%% YoY profit collapse must block, got %+v", v)
	}
	if !strings.Contains(v.Reason, "YoY") {
		t.Errorf("reason should state the comparison basis, got %q", v.Reason)
	}
}

func TestGateBlocksOnLossMakingQuarter(t *testing.T) {
	s := &stubServer{
		news:   newsJSON(testISIN),
		income: incomeJSON("Dec 2025", -5, 95, 90, 85, 100),
	}
	v, err := newGateFor(t, s).Assess("TEST", asOf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Allow {
		t.Fatalf("a loss-making quarter must block, got %+v", v)
	}
}

// Indian quarterly results are seasonal: a quarter-on-quarter dip against a
// strong year-ago comparison is not a collapse, and the gate must not read it
// as one.
func TestGateComparesYearOnYearNotQuarterOnQuarter(t *testing.T) {
	s := &stubServer{
		news: newsJSON(testISIN),
		// Latest 80, previous quarter 200 (−60% QoQ), year-ago 75 (+7% YoY).
		income: incomeJSON("Dec 2025", 80, 200, 150, 120, 75),
	}
	v, err := newGateFor(t, s).Assess("TEST", asOf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Allow {
		t.Fatalf("a seasonal QoQ dip with YoY growth must not block, got %+v", v)
	}
}

func TestGateAllowsCleanGap(t *testing.T) {
	s := &stubServer{
		news: newsJSON(testISIN, NewsItem{
			Heading:       "TEST announces new distribution partnership",
			PublishedTime: hoursAgo(3),
		}),
		income: incomeJSON("Dec 2025", 110, 95, 90, 85, 100),
	}
	v, err := newGateFor(t, s).Assess("TEST", asOf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Allow {
		t.Fatalf("clean news and healthy results must allow the fade, got %+v", v)
	}
}

// Fundamentals older than MaxResultsAgeDays say nothing about today. The gate
// reports that rather than pretending the stale quarter is evidence.
func TestGateTreatsAncientResultsAsUnavailable(t *testing.T) {
	s := &stubServer{
		news: newsJSON(testISIN),
		// A collapse, but reported for a quarter ending in 2019.
		income: incomeJSON("Mar 2019", 10, 95, 90, 85, 100),
	}
	v, err := newGateFor(t, s).Assess("TEST", asOf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !v.Allow || !strings.Contains(v.Reason, "stale") {
		t.Fatalf("stale fundamentals should allow and say so, got %+v", v)
	}
}

func TestGateErrorsWhenSymbolHasNoISIN(t *testing.T) {
	s := &stubServer{news: newsJSON(testISIN), income: incomeJSON("Dec 2025", 100, 90)}
	g := NewGate(s.start(t), mapLookup{}, GateConfig{})
	if _, err := g.Assess("UNKNOWN", asOf); err == nil {
		t.Fatal("an unresolvable symbol must error, not silently pass")
	}
}

func TestGateSurfacesHTTPErrors(t *testing.T) {
	s := &stubServer{newsErr: http.StatusUnauthorized, income: incomeJSON("Dec 2025", 100, 90)}
	if _, err := newGateFor(t, s).Assess("TEST", asOf); err == nil {
		t.Fatal("an expired token must surface as an error so the strategy can fail closed")
	}
}

// One verdict per symbol per day: the strategy takes at most one entry, and
// re-polling two endpoints would only add latency to the candle path.
func TestGateCachesVerdictPerSymbolPerDay(t *testing.T) {
	s := &stubServer{
		news:   newsJSON(testISIN),
		income: incomeJSON("Dec 2025", 110, 95, 90, 85, 100),
	}
	g := newGateFor(t, s)
	if _, err := g.Assess("TEST", asOf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	before := len(s.paths)
	if _, err := g.Assess("TEST", asOf.Add(20*time.Minute)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(s.paths) != before {
		t.Errorf("second same-day call should hit the cache, saw %v", s.paths[before:])
	}
	if _, err := g.Assess("TEST", asOf.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(s.paths) == before {
		t.Error("a new trading day must re-query")
	}
}

// The wire format is documented, not guessed: news is keyed by instrument key
// and fundamentals by ISIN path segment.
func TestClientRequestShape(t *testing.T) {
	s := &stubServer{
		news:   newsJSON(testISIN),
		income: incomeJSON("Dec 2025", 100, 90),
	}
	c := s.start(t)
	if _, err := c.News(testISIN, 25); err != nil {
		t.Fatalf("news: %v", err)
	}
	if _, err := c.IncomeStatementQuarterly(testISIN); err != nil {
		t.Fatalf("income: %v", err)
	}
	if !strings.Contains(s.paths[0], "category=instrument_keys") ||
		!strings.Contains(s.paths[0], "NSE_EQ%7C"+testISIN) {
		t.Errorf("news request malformed: %s", s.paths[0])
	}
	if !strings.Contains(s.paths[1], "/fundamentals/"+testISIN+"/income-statement") ||
		!strings.Contains(s.paths[1], "time_period=quarterly") {
		t.Errorf("income request malformed: %s", s.paths[1])
	}
}

func TestLoadISINCSV(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "list.csv")
	content := "Company Name,Industry,Symbol,Series,ISIN Code\n" +
		"Reliance Industries Ltd.,Oil Gas,RELIANCE,EQ,INE002A01018\n" +
		"Infosys Ltd.,IT,INFY,EQ,INE009A01021\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	l, err := LoadISINCSV(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if l.Len() != 2 {
		t.Errorf("expected 2 mappings, got %d", l.Len())
	}
	if isin, ok := l.ISIN("RELIANCE"); !ok || isin != "INE002A01018" {
		t.Errorf("RELIANCE → %q %v", isin, ok)
	}
	// The watchlist CSVs carry Yahoo-suffixed lowercase symbols; both must
	// resolve to the same row.
	if isin, ok := l.ISIN("infy.ns"); !ok || isin != "INE009A01021" {
		t.Errorf("infy.ns → %q %v", isin, ok)
	}
	if _, ok := l.ISIN("NOTLISTED"); ok {
		t.Error("unknown symbol must not resolve")
	}
}

func TestLoadISINCSVRejectsFileWithoutISINColumn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "beta.csv")
	if err := os.WriteFile(path, []byte("symbol,beta\nRELIANCE,1.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadISINCSV(path); err == nil {
		t.Fatal("a CSV without an ISIN column must fail loudly at startup")
	}
}
