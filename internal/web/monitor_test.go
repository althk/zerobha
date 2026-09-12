package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/internal/risk"
	"zerobha/pkg/broker"
	"zerobha/pkg/db"

	"github.com/shopspring/decimal"
)

// legStrategy is a minimal strategy that reports one open leg.
type legStrategy struct{ legs []core.OpenLeg }

func (legStrategy) Name() string                          { return "emacross" }
func (legStrategy) Init(core.DataProvider) error          { return nil }
func (legStrategy) OnCandle(models.Candle) *models.Signal { return nil }
func (s legStrategy) OpenLegs() []core.OpenLeg            { return s.legs }

// The whole pipeline for one paper option trade: the fill carries the signal
// metadata, charges and the underlying's price; the tracker pairs entry and
// exit into a trade with reason, costs and metadata; the monitor reads it
// back in index bps with the backtest reference beside it.
func TestMonitorReadsAPaperOptionTradeEndToEnd(t *testing.T) {
	store, err := db.NewStore(t.TempDir() + "/m.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	paper := broker.NewPaperAdapter(nil, decimal.NewFromInt(1000000), broker.WithPaperOptionSpread(20))
	engine := core.NewEngine(legStrategy{}, paper, risk.NewManager(nil, decimal.NewFromInt(100000), 50, 10), nil, nil, store)
	engine.PaperMode = true
	srv := NewServer(engine, 0, true)

	// Entry: a put expressing a SHORT index view, as option_leg.go builds it.
	entry := models.Order{
		Symbol: "NIFTY26SEP24300PE", Side: models.BuySignal, Type: "MARKET", ProductType: "MIS", Exchange: "NFO",
		Quantity: decimal.NewFromInt(650), Price: decimal.NewFromInt(200),
		Metadata: map[string]string{"Strategy": "emacross", "Underlying": "NIFTY 50", "IndexSide": "SELL",
			"IndexEntry": "24300.00", "IndexStop": "24400.00", "DaysToExpiry": "3", "Reason": "DeathCross"},
		Timestamp: time.Now().Add(-2 * time.Hour),
	}
	if _, err := paper.PlaceOrder(entry); err != nil {
		t.Fatal(err)
	}
	// The index ticks on the feed; the contract rallies as the index falls.
	paper.OnTick("NIFTY 50", decimal.NewFromFloat(24180), time.Now())
	paper.OnTick("NIFTY26SEP24300PE", decimal.NewFromInt(260), time.Now())
	if _, err := paper.ClosePosition("NIFTY26SEP24300PE", models.BuySignal, decimal.NewFromInt(260), time.Now(), "AutoSquareOff"); err != nil {
		t.Fatal(err)
	}

	// A signal that the engine placed, and one the option layer declined.
	id, _ := store.SaveSignal(&models.Signal{Symbol: "NIFTY 50", Type: models.SellSignal, Metadata: map[string]string{"Strategy": "emacross"}}, true)
	store.RecordSignalOutcome(id, db.SignalPlaced, "PAPER-000001")
	store.SaveDeclinedSignal("SENSEX", "emacross", "BUY", "too close to expiry", true)

	srv.reconcileTrades()

	rec := httptest.NewRecorder()
	srv.handleStrategy(rec, httptest.NewRequest(http.MethodGet, "/api/strategy?days=7", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Funnel struct {
			Total  int `json:"total"`
			Placed int `json:"placed"`
		} `json:"funnel"`
		Overall   groupStats   `json:"overall"`
		ByExit    []groupStats `json:"by_exit"`
		ByDTE     []groupStats `json:"by_dte"`
		Trades    []tradeRow   `json:"trades"`
		Reference []reference  `json:"reference"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}

	if out.Funnel.Total != 2 || out.Funnel.Placed != 1 {
		t.Errorf("funnel = %+v, want 2 signals / 1 placed", out.Funnel)
	}
	if len(out.Trades) != 1 {
		t.Fatalf("want 1 trade, got %d", len(out.Trades))
	}
	tr := out.Trades[0]
	if tr.Side != "SHORT" || tr.Underlying != "NIFTY 50" || tr.ExitReason != "EOD square-off" || tr.DTE != 3 {
		t.Errorf("trade context lost: %+v", tr)
	}
	// Index fell 24300 -> 24180 on a short view: +49.4 bps.
	if !tr.HasIndex || tr.IndexBps < 49 || tr.IndexBps > 50 {
		t.Errorf("index bps = %.2f (has=%v), want ~+49.4", tr.IndexBps, tr.HasIndex)
	}
	// Spread: buy at 200.50, sell at 259.50; gross (259.5-200.5)*650 = 38,350;
	// charges on both legs on top. Net must be below gross by the charges.
	if tr.EntryPrice != 200.5 || tr.ExitPrice != 259.5 {
		t.Errorf("fills = %.2f / %.2f, want 200.50 / 259.50 (half spread each way)", tr.EntryPrice, tr.ExitPrice)
	}
	if tr.Costs <= 0 || tr.PnL >= tr.GrossPnL || tr.GrossPnL != 38350 {
		t.Errorf("gross %.2f costs %.2f net %.2f", tr.GrossPnL, tr.Costs, tr.PnL)
	}
	if out.Overall.N != 1 || out.Overall.IndexN != 1 || out.Overall.CostsRupees != tr.Costs {
		t.Errorf("overall = %+v", out.Overall)
	}
	if len(out.ByExit) != 1 || out.ByExit[0].Group != "EOD square-off" {
		t.Errorf("by_exit = %+v", out.ByExit)
	}
	if len(out.ByDTE) != 1 || out.ByDTE[0].Group != "DTE 3" {
		t.Errorf("by_dte = %+v", out.ByDTE)
	}
	if len(out.Reference) == 0 || out.Reference[0].IndexBps != 2.53 {
		t.Errorf("reference for emacross missing: %+v", out.Reference)
	}
}

// The positions view carries the index stop the strategy holds for a leg —
// the one exit no broker can show.
func TestPositionsCarryTheStrategysIndexStop(t *testing.T) {
	paper := broker.NewPaperAdapter(nil, decimal.NewFromInt(1000000), broker.WithoutPaperCharges())
	strat := legStrategy{legs: []core.OpenLeg{{
		Symbol: "NIFTY26SEP24300PE", Underlying: "NIFTY 50", Side: "SHORT",
		IndexEntry: decimal.NewFromInt(24300), IndexStop: decimal.NewFromInt(24400), IndexBest: decimal.NewFromInt(24250),
	}}}
	engine := core.NewEngine(strat, paper, risk.NewManager(nil, decimal.NewFromInt(100000), 50, 10), nil, nil, nil)
	srv := NewServer(engine, 0, true)

	paper.PlaceOrder(models.Order{Symbol: "NIFTY26SEP24300PE", Side: models.BuySignal, ProductType: "MIS", Exchange: "NFO",
		Quantity: decimal.NewFromInt(65), Price: decimal.NewFromInt(200), Metadata: map[string]string{"Underlying": "NIFTY 50"}})
	paper.OnTick("NIFTY 50", decimal.NewFromInt(24250), time.Now())

	rec := httptest.NewRecorder()
	srv.handlePositions(rec, httptest.NewRequest(http.MethodGet, "/api/positions", nil))
	var views []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil || len(views) != 1 {
		t.Fatalf("decode: %v (%d rows) %s", err, len(views), rec.Body.String())
	}
	v := views[0]
	if v["index_stop"] != "24400" || v["index_side"] != "SHORT" || v["underlying"] != "NIFTY 50" {
		t.Errorf("position view missing the leg: %v", v)
	}
}
