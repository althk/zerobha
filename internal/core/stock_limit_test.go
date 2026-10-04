package core

import (
	"testing"
	"time"

	"zerobha/internal/models"
	"zerobha/internal/risk"
	"zerobha/pkg/broker"
	"zerobha/pkg/db"

	"github.com/shopspring/decimal"
)

// scriptedSignals emits signals[i] on the i-th candle, nil once they run out.
type scriptedSignals struct {
	signals []*models.Signal
	i       int
}

func (s *scriptedSignals) Name() string            { return "scripted" }
func (s *scriptedSignals) Init(DataProvider) error { return nil }
func (s *scriptedSignals) OnCandle(models.Candle) *models.Signal {
	if s.i >= len(s.signals) {
		return nil
	}
	sig := s.signals[s.i]
	s.i++
	return sig
}

func limitTestEngine(t *testing.T, perStock int64, sigs ...*models.Signal) (*Engine, *broker.PaperAdapter) {
	t.Helper()
	paper := broker.NewPaperAdapter(nil, decimal.NewFromInt(1000000), broker.WithoutPaperCharges())
	rm := risk.NewManager(nil, decimal.Zero, 100, 0)
	rm.MaxLossPerStockPerDay = decimal.NewFromInt(perStock)
	e := NewEngine(&scriptedSignals{signals: sigs}, paper, rm, nil, nil, nil)
	e.UptrendOnly = false
	e.MaxCapitalPerTrade = 500000
	return e, paper
}

var limitStart = time.Date(2026, 10, 5, 10, 0, 0, 0, time.FixedZone("IST", 5*3600+1800))

func limitBar(i int) models.Candle {
	st := limitStart.Add(time.Duration(i) * 5 * time.Minute)
	return models.Candle{Symbol: "X", Close: decimal.NewFromInt(100), StartTime: st, EndTime: st.Add(5 * time.Minute)}
}

func openQty(t *testing.T, b *broker.PaperAdapter, symbol string) int {
	t.Helper()
	positions, err := b.GetPositions()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range positions {
		if p.Tradingsymbol == symbol {
			return p.NetQuantity
		}
	}
	return 0
}

func equitySignal(symbol string, price, stop int64) *models.Signal {
	return &models.Signal{Symbol: symbol, Type: models.BuySignal, Price: decimal.NewFromInt(price),
		StopLoss: decimal.NewFromInt(stop), ProductType: "MIS", Exchange: "NSE",
		Metadata: map[string]string{"Strategy": "scripted"}}
}

// Rs10L over 5 slots is Rs2L a trade; the sizer's 1% at Rs10 a share is 200
// shares, and a Rs1,500 per-stock budget allows 150.
func TestPerStockLimitCapsQuantity(t *testing.T) {
	e, paper := limitTestEngine(t, 1500, equitySignal("SBIN", 500, 490))
	e.Execute(limitBar(0))
	if q := openQty(t, paper, "SBIN"); q != 150 {
		t.Errorf("SBIN quantity = %d, want 150 (Rs1,500 budget / Rs10 risk)", q)
	}
}

// Two option legs on the same index, as two strategies would open them, share
// one budget: the second is cut to what the first left.
func TestPerStockLimitIsSharedAcrossContractsOfOneUnderlying(t *testing.T) {
	leg := func(symbol string, riskPct float64) *models.Signal {
		s := &models.Signal{Symbol: symbol, Type: models.BuySignal, Price: decimal.NewFromInt(100),
			StopLoss: decimal.NewFromInt(80), ProductType: "MIS", Exchange: "NFO",
			Metadata: map[string]string{"Strategy": "scripted", "Underlying": "NIFTY 50"}}
		if riskPct > 0 {
			s.RiskPct = decimal.NewFromFloat(riskPct)
		}
		return s
	}
	// CE: 0.2% of Rs2L = Rs400 / Rs20 = 20 units, Rs400 at risk.
	// PE: 1% would be ~124 units; Rs1,600 of the Rs2,000 budget is left -> 80.
	// SENSEX: a different underlying, its own full budget -> 100.
	sensex := leg("SENSEX26OCT80000CE", 0)
	sensex.Metadata = map[string]string{"Strategy": "scripted", "Underlying": "SENSEX"}
	e, paper := limitTestEngine(t, 2000, leg("NIFTY26OCT24000CE", 0.002), leg("NIFTY26OCT24500PE", 0), sensex)

	e.Execute(limitBar(0))
	e.Execute(limitBar(1))
	e.Execute(limitBar(2))
	if q := openQty(t, paper, "NIFTY26OCT24000CE"); q != 20 {
		t.Errorf("CE quantity = %d, want 20", q)
	}
	if q := openQty(t, paper, "NIFTY26OCT24500PE"); q != 80 {
		t.Errorf("PE quantity = %d, want 80 (cut to the budget the CE left)", q)
	}
	if q := openQty(t, paper, "SENSEX26OCT80000CE"); q != 100 {
		t.Errorf("SENSEX quantity = %d, want 100 (its own Rs2,000 budget)", q)
	}
}

// A stock that has lost its budget for the day takes no more entries; other
// stocks are unaffected.
func TestPerStockLimitBlocksAStockAfterItsLoss(t *testing.T) {
	e, paper := limitTestEngine(t, 1500,
		equitySignal("SBIN", 500, 490), equitySignal("SBIN", 500, 490), equitySignal("TCS", 500, 490))

	e.Execute(limitBar(0)) // 150 SBIN
	paper.OnTick("SBIN", decimal.NewFromInt(480), limitStart.Add(6*time.Minute))
	if q := openQty(t, paper, "SBIN"); q != 0 {
		t.Fatalf("precondition: SBIN stop should have filled, qty %d", q)
	}

	e.Execute(limitBar(1)) // SBIN again: refused
	if q := openQty(t, paper, "SBIN"); q != 0 {
		t.Errorf("SBIN re-entry should be blocked after a Rs3,000 loss on a Rs1,500 limit, qty %d", q)
	}
	e.Execute(limitBar(2)) // TCS: allowed
	if q := openQty(t, paper, "TCS"); q != 150 {
		t.Errorf("TCS quantity = %d, want 150 (its own budget)", q)
	}
}

func TestCapQuantityToRiskRoundsToLots(t *testing.T) {
	s := &models.Signal{Price: decimal.NewFromInt(200), StopLoss: decimal.NewFromInt(150), LotSize: 65}
	// Rs10,000 / Rs50 = 200 units -> 3 lots of 65 = 195.
	if got := capQuantityToRisk(decimal.NewFromInt(650), s, decimal.NewFromInt(10000)); !got.Equal(decimal.NewFromInt(195)) {
		t.Errorf("capped = %s, want 195", got)
	}
	// No stop: the whole price is at risk.
	s.StopLoss = decimal.Zero
	if got := capQuantityToRisk(decimal.NewFromInt(650), s, decimal.NewFromInt(10000)); !got.Equal(decimal.Zero) {
		t.Errorf("capped = %s, want 0 (50 units is under one lot)", got)
	}
}

// The monthly limit adds the month's earlier sessions, read from the trade
// journal, to today's broker PnL. A loss earlier in the month past the limit
// blocks today's first entry; last month's losses do not count.
func TestMonthlyLimitReadsEarlierSessionsFromTheJournal(t *testing.T) {
	store, err := db.NewStore(t.TempDir() + "/month.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ist := limitStart.Location()
	save := func(key string, exit time.Time, pnl int64) {
		if err := store.SaveTrade(key, models.Trade{Symbol: "SBIN", PnL: decimal.NewFromInt(pnl),
			EntryTime: exit.Add(-time.Hour), ExitTime: exit}); err != nil {
			t.Fatal(err)
		}
	}
	save("last-month", time.Date(2026, 9, 29, 11, 0, 0, 0, ist), -90000)

	run := func() int {
		paper := broker.NewPaperAdapter(nil, decimal.NewFromInt(1000000), broker.WithoutPaperCharges())
		rm := risk.NewManager(nil, decimal.Zero, 100, 0)
		rm.MaxMonthlyLoss = decimal.NewFromInt(50000)
		e := NewEngine(&scriptedSignals{signals: []*models.Signal{equitySignal("SBIN", 500, 490)}}, paper, rm, nil, nil, store)
		e.UptrendOnly = false
		e.Execute(limitBar(0))
		return openQty(t, paper, "SBIN")
	}
	if q := run(); q == 0 {
		t.Fatal("last month's loss must not count against this month")
	}
	save("this-month", time.Date(2026, 10, 1, 11, 0, 0, 0, ist), -60000)
	if q := run(); q != 0 {
		t.Errorf("a Rs60,000 loss on Oct 1 should block entries on Oct 5 under a Rs50,000 monthly limit, qty %d", q)
	}
}
