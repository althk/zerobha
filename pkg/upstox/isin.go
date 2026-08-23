package upstox

import (
	"encoding/csv"
	"fmt"
	"os"
	"strings"
)

// ISINLookup resolves an NSE trading symbol to its ISIN.
//
// Upstox keys both instrument news and the fundamentals endpoints by ISIN,
// while every other part of this system speaks trading symbols. The mapping is
// static enough to come from a file rather than an API call: the NSE index
// constituent CSVs already in the repo carry both columns.
type ISINLookup interface {
	ISIN(symbol string) (string, bool)
}

// CSVISINLookup is an in-memory symbol → ISIN map loaded from an NSE index
// constituent CSV. It is read once at startup and never mutated afterwards, so
// concurrent lookups are safe.
type CSVISINLookup struct {
	bySymbol map[string]string
}

// LoadISINCSV reads an NSE constituent CSV (columns "Symbol" and "ISIN Code",
// as published for ind_nifty50list.csv / ind_nifty200list.csv /
// ind_nifty500list.csv) into a lookup.
//
// Symbols are normalised the same way the rest of the system normalises them:
// uppercased with any ".NS" Yahoo suffix removed.
func LoadISINCSV(path string) (*CSVISINLookup, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("isin csv: %w", err)
	}
	defer func() { _ = f.Close() }()

	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("isin csv %s: %w", path, err)
	}
	if len(records) < 2 {
		return nil, fmt.Errorf("isin csv %s: empty or missing header", path)
	}

	symbolIdx, isinIdx := -1, -1
	for i, col := range records[0] {
		switch strings.ToLower(strings.TrimSpace(col)) {
		case "symbol":
			symbolIdx = i
		case "isin code", "isin":
			isinIdx = i
		}
	}
	if symbolIdx == -1 || isinIdx == -1 {
		return nil, fmt.Errorf("isin csv %s: needs 'Symbol' and 'ISIN Code' columns, got %v", path, records[0])
	}

	m := make(map[string]string, len(records))
	for _, rec := range records[1:] {
		if len(rec) <= symbolIdx || len(rec) <= isinIdx {
			continue
		}
		sym := NormalizeSymbol(rec[symbolIdx])
		isin := strings.TrimSpace(rec[isinIdx])
		if sym == "" || isin == "" {
			continue
		}
		m[sym] = isin
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("isin csv %s: no usable rows", path)
	}
	return &CSVISINLookup{bySymbol: m}, nil
}

// ISIN returns the ISIN for a trading symbol.
func (l *CSVISINLookup) ISIN(symbol string) (string, bool) {
	isin, ok := l.bySymbol[NormalizeSymbol(symbol)]
	return isin, ok
}

// Len reports how many symbols were loaded, for startup logging.
func (l *CSVISINLookup) Len() int { return len(l.bySymbol) }

// NormalizeSymbol uppercases a symbol and strips the ".NS" Yahoo suffix, the
// same normalisation the backtester's symbol loader applies.
func NormalizeSymbol(s string) string {
	return strings.TrimSuffix(strings.ToUpper(strings.TrimSpace(s)), ".NS")
}
