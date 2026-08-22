// Command histdl downloads historical candles from the Zerodha Kite API into
// the same test/data/<interval>/<symbol>_real.csv layout the backtester reads.
//
// Yahoo (scripts/data_downloader.py) caps intraday history at 60 days, which is
// too short to evaluate a strategy that trades a few times a week. Kite serves
// years of intraday data, so this tool exists to feed the backtester a sample
// large enough to be significant.
//
// It requires a Kite Connect subscription with the historical data add-on, and
// an interactive login: the tool prints a login URL, waits for the callback on
// :9880, and caches the resulting access token for the rest of the day.
//
// Usage:
//
//	go run ./cmd/histdl -csv high_beta_100.csv -interval 5minute -days 400
//	go run ./cmd/histdl -csv watchlist.csv -interval day -days 1000 -limit 50
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"zerobha/internal/config"
)

// chunkDays is the maximum span the Kite historical API serves per request,
// per interval. Kite rejects wider ranges outright, so long histories are
// fetched as a series of chunks and concatenated.
func chunkDays(interval string) int {
	switch interval {
	case "minute":
		return 55
	case "3minute", "5minute", "10minute":
		return 90
	case "15minute", "30minute", "60minute":
		return 180
	default: // day
		return 1800
	}
}

// tokenCache persists the access token so repeated runs on the same day skip
// the browser login. Kite invalidates tokens each morning, so a cache from a
// previous day is discarded.
type tokenCache struct {
	Date        string `json:"date"`
	AccessToken string `json:"access_token"`
}

const tokenCachePath = ".kite_token.json"

func loadCachedToken() string {
	b, err := os.ReadFile(tokenCachePath)
	if err != nil {
		return ""
	}
	var tc tokenCache
	if err := json.Unmarshal(b, &tc); err != nil {
		return ""
	}
	if tc.Date != time.Now().Format("2006-01-02") {
		return ""
	}
	return tc.AccessToken
}

func saveCachedToken(token string) {
	b, err := json.Marshal(tokenCache{Date: time.Now().Format("2006-01-02"), AccessToken: token})
	if err != nil {
		return
	}
	if err := os.WriteFile(tokenCachePath, b, 0600); err != nil {
		log.Printf("WARNING: could not cache access token: %v", err)
	}
}

// fetchRequestToken runs the interactive Kite login: the user opens the printed
// URL, logs in, and Kite redirects to the local callback with a request token.
func fetchRequestToken(loginURL string) (string, error) {
	fmt.Println("Open the following url in your browser:\n", loginURL)

	srv := &http.Server{Addr: ":9880"}
	var requestToken string
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/kite/callback", func(w http.ResponseWriter, r *http.Request) {
		tokens := r.URL.Query()["request_token"]
		if len(tokens) > 0 {
			requestToken = tokens[0]
		}
		go func() { _ = srv.Shutdown(context.TODO()) }()
		_, _ = w.Write([]byte("login successful! you can close this tab."))
	})
	srv.Handler = mux

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return "", err
	}
	if requestToken == "" {
		return "", fmt.Errorf("no request token received on callback")
	}
	return requestToken, nil
}

func login(cfg *config.Config) (*kiteconnect.Client, error) {
	kc := kiteconnect.New(cfg.APIKey)

	if cached := loadCachedToken(); cached != "" {
		kc.SetAccessToken(cached)
		if _, err := kc.GetUserProfile(); err == nil {
			log.Println("Reusing cached access token.")
			return kc, nil
		}
		log.Println("Cached token rejected, starting a fresh login.")
	}

	requestToken, err := fetchRequestToken(kc.GetLoginURL())
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	data, err := kc.GenerateSession(requestToken, cfg.APISecret)
	if err != nil {
		return nil, fmt.Errorf("generate session: %w", err)
	}
	kc.SetAccessToken(data.AccessToken)
	saveCachedToken(data.AccessToken)
	return kc, nil
}

func fetchNSETokens(kc *kiteconnect.Client) (map[string]uint32, error) {
	instruments, err := kc.GetInstrumentsByExchange("NSE")
	if err != nil {
		return nil, err
	}
	out := make(map[string]uint32, len(instruments))
	for _, inst := range instruments {
		if inst.Exchange != "NSE" {
			continue
		}
		out[inst.Tradingsymbol] = uint32(inst.InstrumentToken)
	}
	return out, nil
}

// downloadSymbol pulls `days` of history in API-sized chunks and writes one CSV
// in the layout cmd/backtest expects.
func downloadSymbol(kc *kiteconnect.Client, symbol string, token uint32, interval string, days int, outDir string, pause time.Duration) (int, error) {
	to := time.Now()
	from := to.AddDate(0, 0, -days)
	span := chunkDays(interval)

	type row struct {
		ts                        time.Time
		open, high, low, closeVal float64
		volume                    int
	}
	var rows []row

	for start := from; start.Before(to); start = start.AddDate(0, 0, span) {
		end := start.AddDate(0, 0, span)
		if end.After(to) {
			end = to
		}

		data, err := kc.GetHistoricalData(int(token), interval, start, end, false, false)
		if err != nil {
			return 0, fmt.Errorf("%s %s..%s: %w", symbol,
				start.Format("2006-01-02"), end.Format("2006-01-02"), err)
		}
		for _, c := range data {
			rows = append(rows, row{c.Date.Time, c.Open, c.High, c.Low, c.Close, c.Volume})
		}
		// Kite rate-limits historical requests; pace them.
		time.Sleep(pause)
	}

	if len(rows) == 0 {
		return 0, nil
	}

	if err := os.MkdirAll(outDir, 0755); err != nil {
		return 0, err
	}
	path := filepath.Join(outDir, strings.ToLower(symbol)+"_real.csv")
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"timestamp", "open", "high", "low", "close", "volume"}); err != nil {
		return 0, err
	}
	for _, r := range rows {
		if err := w.Write([]string{
			r.ts.Format(time.RFC3339),
			fmt.Sprintf("%.2f", r.open),
			fmt.Sprintf("%.2f", r.high),
			fmt.Sprintf("%.2f", r.low),
			fmt.Sprintf("%.2f", r.closeVal),
			fmt.Sprintf("%d", r.volume),
		}); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}

// loadSymbols reads a watchlist CSV, accepting either a "symbol"/"Symbol"
// column (the format find_high_beta.py and build_watchlist.py emit) or a bare
// first column, and strips any ".NS" suffix.
func loadSymbols(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(records) < 2 {
		return nil, fmt.Errorf("%s: empty or header-only", path)
	}

	idx := 0
	for i, col := range records[0] {
		if strings.EqualFold(col, "symbol") {
			idx = i
			break
		}
	}

	var out []string
	for _, rec := range records[1:] {
		if len(rec) <= idx {
			continue
		}
		sym := strings.ToUpper(strings.TrimSuffix(strings.TrimSpace(rec[idx]), ".NS"))
		if sym != "" {
			out = append(out, sym)
		}
	}
	return out, nil
}

func main() {
	csvFile := flag.String("csv", "high_beta_stocks.csv", "Watchlist CSV to download")
	interval := flag.String("interval", "5minute", "Kite interval: minute, 3minute, 5minute, 15minute, 30minute, 60minute, day")
	days := flag.Int("days", 400, "How many calendar days of history to fetch")
	limit := flag.Int("limit", 0, "Only download the first N symbols (0 = all)")
	outRoot := flag.String("out", "test/data", "Root directory for the downloaded CSVs")
	pauseMs := flag.Int("pause-ms", 350, "Delay between API requests (Kite rate-limits historical calls)")
	configFile := flag.String("config", "config.local.toml", "TOML config supplying api_key/api_secret")
	flag.Parse()

	cfg, err := config.LoadConfig(*configFile)
	if err != nil {
		log.Fatalf("Failed to load config %s: %v", *configFile, err)
	}

	symbols, err := loadSymbols(*csvFile)
	if err != nil {
		log.Fatalf("Failed to load symbols: %v", err)
	}
	if *limit > 0 && len(symbols) > *limit {
		symbols = symbols[:*limit]
	}
	fmt.Printf("Downloading %s history for %d symbols (%d days)...\n", *interval, len(symbols), *days)

	kc, err := login(cfg)
	if err != nil {
		log.Fatalf("Kite login failed: %v", err)
	}

	tokens, err := fetchNSETokens(kc)
	if err != nil {
		log.Fatalf("Failed to fetch instruments: %v", err)
	}
	fmt.Printf("Loaded %d NSE instruments.\n", len(tokens))

	outDir := filepath.Join(*outRoot, *interval)
	pause := time.Duration(*pauseMs) * time.Millisecond

	var ok, failed int
	for i, sym := range symbols {
		token, found := tokens[sym]
		if !found {
			log.Printf("[%d/%d] %s: not in the NSE instrument list, skipping", i+1, len(symbols), sym)
			failed++
			continue
		}

		n, err := downloadSymbol(kc, sym, token, *interval, *days, outDir, pause)
		if err != nil {
			log.Printf("[%d/%d] %s: %v", i+1, len(symbols), sym, err)
			failed++
			continue
		}
		if n == 0 {
			log.Printf("[%d/%d] %s: no candles returned", i+1, len(symbols), sym)
			failed++
			continue
		}
		fmt.Printf("[%d/%d] %s: %d candles -> %s\n", i+1, len(symbols), sym, n,
			filepath.Join(outDir, strings.ToLower(sym)+"_real.csv"))
		ok++
	}

	fmt.Printf("\nDone: %d symbols downloaded, %d failed/skipped. Data in %s\n", ok, failed, outDir)
}
