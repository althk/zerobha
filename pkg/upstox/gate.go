package upstox

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"zerobha/internal/core"
)

// Gate implements core.NewsGate over the Upstox news and fundamentals
// endpoints: it answers "is this gap down explained by genuinely bad
// information?" so a gap-fade strategy can decline to catch that particular
// knife.
//
// Two independent vetoes, either of which blocks the trade:
//
//   - News. Any headline or summary published within NewsLookback that
//     contains a blocking keyword (fraud, probe, resignation, default,
//     downgrade, guidance cut, …).
//   - Earnings. The latest reported quarter is loss-making, or net profit fell
//     by more than MaxProfitDropPct — measured year-on-year where the history
//     reaches back four quarters, because Indian quarterly results are
//     seasonal and a QoQ comparison misreads that seasonality as collapse.
//
// What it deliberately does NOT do is decide that good news means buy. The
// strategy's edge, if any, is in fading *unexplained* panics; this gate only
// removes the explained ones.
//
// Both sources are current-state APIs with no as-of-date parameter, so a Gate
// can only be used live. A backtest must run the strategy with a nil gate and
// is therefore testing the ungated variant — see the note on strategy.GapFade.
type Gate struct {
	client *Client
	isin   ISINLookup
	cfg    GateConfig

	mu    sync.Mutex
	cache map[string]core.GateVerdict // key: symbol|YYYY-MM-DD
}

// GateConfig is the policy the Gate applies to the fetched data.
type GateConfig struct {
	// NewsLookback bounds how far back a headline still counts as an
	// explanation for today's gap.
	NewsLookback time.Duration
	// BlockKeywords are matched case-insensitively as substrings of the
	// headline and summary. Empty uses DefaultBlockKeywords.
	BlockKeywords []string
	// MaxProfitDropPct blocks when net profit fell by more than this percent.
	MaxProfitDropPct float64
	// MaxResultsAgeDays bounds how old the latest reported quarter may be
	// before the fundamentals are treated as stale and simply unavailable.
	MaxResultsAgeDays int
	// NewsPageSize caps how many articles are pulled per symbol.
	NewsPageSize int
}

// DefaultBlockKeywords is the built-in list of phrases that mark a gap down as
// informed rather than a panic. It is deliberately blunt and errs toward
// blocking: a missed trade costs nothing, and this strategy's whole premise is
// that the *unexplained* gaps are the tradeable ones.
var DefaultBlockKeywords = []string{
	"fraud", "scam", "probe", "investigation", "raid", "search and seizure",
	"sebi", "enforcement directorate", "income tax department", "cbi",
	"insolvency", "nclt", "bankrupt", "default", "downgrade", "downgrades",
	"resign", "resigns", "resignation", "steps down", "quits",
	"auditor", "qualified opinion", "restated", "accounting",
	"guidance cut", "cuts guidance", "profit warning", "warns",
	"plunge", "slump", "misses estimates", "miss estimates", "disappoint",
	"loss widens", "swings to loss", "posts loss", "net loss",
	"recall", "ban", "banned", "penalty", "fine", "lawsuit", "class action",
	"stake sale", "block deal", "pledge", "promoter selling",
	"fire", "accident", "shutdown", "strike", "halt",
}

// NewGate builds a Gate. isin resolves trading symbols to the ISINs Upstox
// keys its data by.
func NewGate(client *Client, isin ISINLookup, cfg GateConfig) *Gate {
	if cfg.NewsLookback <= 0 {
		cfg.NewsLookback = 48 * time.Hour
	}
	if len(cfg.BlockKeywords) == 0 {
		cfg.BlockKeywords = DefaultBlockKeywords
	}
	if cfg.MaxProfitDropPct <= 0 {
		cfg.MaxProfitDropPct = 25
	}
	if cfg.MaxResultsAgeDays <= 0 {
		cfg.MaxResultsAgeDays = 180
	}
	if cfg.NewsPageSize <= 0 {
		cfg.NewsPageSize = 25
	}
	return &Gate{
		client: client,
		isin:   isin,
		cfg:    cfg,
		cache:  make(map[string]core.GateVerdict),
	}
}

// Assess implements core.NewsGate. Verdicts are cached per symbol per trading
// day: the strategy takes at most one gap-fade entry per symbol per day, and
// re-polling two endpoints for a decision already made would only add latency
// to the candle path.
//
// An error means "no verdict" — the strategy decides whether that fails open
// or closed. A missing ISIN is an error, not a silent pass: a symbol the gate
// cannot look up is a symbol whose gap is unexplained only by ignorance.
func (g *Gate) Assess(symbol string, asOf time.Time) (core.GateVerdict, error) {
	key := NormalizeSymbol(symbol) + "|" + asOf.Format("2006-01-02")

	g.mu.Lock()
	cached, ok := g.cache[key]
	g.mu.Unlock()
	if ok {
		return cached, nil
	}

	isin, ok := g.isin.ISIN(symbol)
	if !ok {
		return core.GateVerdict{}, fmt.Errorf("upstox gate: no ISIN for %s", symbol)
	}

	newsVerdict, err := g.assessNews(isin, asOf)
	if err != nil {
		return core.GateVerdict{}, err
	}
	if !newsVerdict.Allow {
		g.remember(key, newsVerdict)
		return newsVerdict, nil
	}

	earningsVerdict, err := g.assessEarnings(isin, asOf)
	if err != nil {
		return core.GateVerdict{}, err
	}

	verdict := earningsVerdict
	if verdict.Allow {
		verdict.Reason = newsVerdict.Reason + "; " + earningsVerdict.Reason
	}
	g.remember(key, verdict)
	return verdict, nil
}

func (g *Gate) remember(key string, v core.GateVerdict) {
	g.mu.Lock()
	g.cache[key] = v
	g.mu.Unlock()
}

// assessNews blocks when a recent headline carries a blocking keyword.
func (g *Gate) assessNews(isin string, asOf time.Time) (core.GateVerdict, error) {
	items, err := g.client.News(isin, g.cfg.NewsPageSize)
	if err != nil {
		return core.GateVerdict{}, err
	}

	cutoff := asOf.Add(-g.cfg.NewsLookback)
	recent := 0
	for _, item := range items {
		published := item.PublishedAt()
		// Articles published after the decision timestamp are not evidence
		// about it; skip them rather than let a later report justify or veto
		// an earlier entry.
		if published.Before(cutoff) || published.After(asOf) {
			continue
		}
		recent++
		if hit := matchKeyword(item.Heading+" "+item.Summary, g.cfg.BlockKeywords); hit != "" {
			return core.GateVerdict{
				Allow:  false,
				Reason: fmt.Sprintf("news %q matched %q", truncate(item.Heading, 90), hit),
			}, nil
		}
	}
	return core.GateVerdict{
		Allow:  true,
		Reason: fmt.Sprintf("news clean (%d recent items)", recent),
	}, nil
}

// assessEarnings blocks when the latest reported quarter is loss-making or its
// net profit fell more than the configured threshold.
func (g *Gate) assessEarnings(isin string, asOf time.Time) (core.GateVerdict, error) {
	stmt, err := g.client.IncomeStatementQuarterly(isin)
	if err != nil {
		return core.GateVerdict{}, err
	}

	history := stmt.Category("net profit")
	if len(history) == 0 {
		// No fundamentals for this name. Absence of bad data is not bad data:
		// the news check already ran, so let the trade through and say so.
		return core.GateVerdict{Allow: true, Reason: "earnings unavailable"}, nil
	}

	latest := history[0]
	if end, ok := parsePeriodEnd(latest.Period); ok {
		age := asOf.Sub(end)
		if age > time.Duration(g.cfg.MaxResultsAgeDays)*24*time.Hour {
			return core.GateVerdict{
				Allow:  true,
				Reason: fmt.Sprintf("earnings stale (latest %s)", latest.Period),
			}, nil
		}
	}

	if latest.Value < 0 {
		return core.GateVerdict{
			Allow:  false,
			Reason: fmt.Sprintf("latest quarter %s is a loss (%.1f cr)", latest.Period, latest.Value),
		}, nil
	}

	// Prefer year-on-year: Indian quarterly results are seasonal, and a
	// quarter-on-quarter fall is routinely seasonality rather than damage.
	dropPct, basis, ok := profitDrop(history)
	if !ok {
		return core.GateVerdict{
			Allow:  true,
			Reason: fmt.Sprintf("net profit %.1f cr (%s), no comparable period", latest.Value, latest.Period),
		}, nil
	}
	if dropPct > g.cfg.MaxProfitDropPct {
		return core.GateVerdict{
			Allow: false,
			Reason: fmt.Sprintf("net profit %s %.1f%% (%s, threshold %.1f%%)",
				basis, dropPct, latest.Period, g.cfg.MaxProfitDropPct),
		}, nil
	}
	return core.GateVerdict{
		Allow:  true,
		Reason: fmt.Sprintf("net profit %s %+.1f%% (%s)", basis, -dropPct, latest.Period),
	}, nil
}

// profitDrop returns how far net profit fell as a positive percent (a rise is
// negative), which comparison it used, and whether a comparison was possible.
// History is newest first, so index 4 is the year-ago quarter.
func profitDrop(history []IncomeHistory) (float64, string, bool) {
	latest := history[0]
	if len(history) >= 5 {
		yearAgo := history[4].Value
		if yearAgo > 0 {
			return (yearAgo - latest.Value) / yearAgo * 100, "YoY", true
		}
	}
	if len(history) >= 2 {
		prev := history[1].Value
		if prev > 0 {
			return (prev - latest.Value) / prev * 100, "QoQ", true
		}
	}
	return 0, "", false
}

// matchKeyword returns the first blocking keyword found in text, or "".
func matchKeyword(text string, keywords []string) string {
	lower := strings.ToLower(text)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return kw
		}
	}
	return ""
}

// parsePeriodEnd interprets the period labels the fundamentals API uses
// ("Jun 2026", "Mar 2025") as the last instant of that month. Anything it does
// not recognise reports false, and the caller treats the age as unknown rather
// than guessing.
func parsePeriodEnd(period string) (time.Time, bool) {
	period = strings.TrimSpace(period)
	for _, layout := range []string{"Jan 2006", "January 2006", "Jan-06", "Jan-2006", "2006-01"} {
		if t, err := time.Parse(layout, period); err == nil {
			// End of that month.
			return t.AddDate(0, 1, 0).Add(-time.Nanosecond), true
		}
	}
	return time.Time{}, false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
