package core

import (
	"testing"
	"time"

	"zerobha/internal/models"
	"zerobha/internal/risk"
	"zerobha/pkg/broker"

	"github.com/shopspring/decimal"
)

// signalEveryBar emits the same BUY signal on every candle.
type signalEveryBar struct{ sig models.Signal }

func (s *signalEveryBar) Name() string            { return "always" }
func (s *signalEveryBar) Init(DataProvider) error { return nil }
func (s *signalEveryBar) OnCandle(models.Candle) *models.Signal {
	sig := s.sig
	return &sig
}

// The daily-loss limit used to compare against a PnL the engine only ever
// updated with zero, so it never tripped — live or paper. It now reads the
// day's PnL from the broker before every signal. Driven through the real
// paper broker: a stop-out past the limit blocks the next entry.
func TestDailyLossLimitBlocksEntriesAfterALosingDay(t *testing.T) {
	paper := broker.NewPaperAdapter(nil, decimal.NewFromInt(1000000), broker.WithoutPaperCharges())
	strat := &signalEveryBar{sig: models.Signal{
		Symbol: "SBIN", Type: models.BuySignal, Price: decimal.NewFromInt(500),
		StopLoss: decimal.NewFromInt(490), ProductType: "MIS", Exchange: "NSE",
		Metadata: map[string]string{"Strategy": "always"},
	}}
	e := NewEngine(strat, paper, risk.NewManager(nil, decimal.NewFromInt(5000), 100, 0), nil, nil, nil)
	e.UptrendOnly = false
	e.MaxCapitalPerTrade = 500000

	start := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	bar := func(i int) models.Candle {
		st := start.Add(time.Duration(i) * 5 * time.Minute)
		return models.Candle{Symbol: "SBIN", Close: decimal.NewFromInt(500), StartTime: st, EndTime: st.Add(5 * time.Minute)}
	}

	// Bar 1: the entry fills.
	e.Execute(bar(1))
	positions, _ := paper.GetPositions()
	if len(positions) != 1 || positions[0].NetQuantity == 0 {
		t.Fatalf("expected an open position after bar 1, got %+v", positions)
	}
	qty := positions[0].NetQuantity

	// The stop fires well past the limit: -Rs60 a share on a position of qty.
	paper.OnTick("SBIN", decimal.NewFromInt(440), start.Add(6*time.Minute))
	positions, _ = paper.GetPositions()
	if positions[0].NetQuantity != 0 {
		t.Fatalf("stop should have closed the position, got %+v", positions[0])
	}
	loss := decimal.NewFromInt(int64(qty)).Mul(decimal.NewFromInt(60)).Neg()
	if !positions[0].PnL.Equal(loss) || loss.GreaterThan(decimal.NewFromInt(-5000)) {
		t.Fatalf("precondition: day loss %s should exceed the Rs5,000 limit", positions[0].PnL)
	}

	// Bar 2: the same signal must be refused by the kill switch.
	e.Execute(bar(2))
	positions, _ = paper.GetPositions()
	for _, p := range positions {
		if p.NetQuantity != 0 {
			t.Fatalf("daily loss limit should have blocked the re-entry, but a position is open: %+v", p)
		}
	}
	if pnl, _ := e.dayPnL(); !pnl.Equal(loss) {
		t.Errorf("engine reads day PnL %s, want %s", pnl, loss)
	}
}

// The simulator books realised PnL per replay date so the same limit means
// the same thing in a backtest.
func TestSimBrokerReportsSessionRealizedPnL(t *testing.T) {
	sim := broker.NewSimBroker(decimal.NewFromInt(500000))
	day1 := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	order := models.Order{Symbol: "SBIN", Side: models.BuySignal, Quantity: decimal.NewFromInt(100),
		Price: decimal.NewFromInt(500), StopLoss: decimal.NewFromInt(490), ProductType: "MIS", Timestamp: day1}
	if _, err := sim.PlaceOrder(order); err != nil {
		t.Fatal(err)
	}
	sim.CheckExits(models.Candle{Symbol: "SBIN", Open: decimal.NewFromInt(495), High: decimal.NewFromInt(495),
		Low: decimal.NewFromInt(480), Close: decimal.NewFromInt(485), StartTime: day1.Add(5 * time.Minute), EndTime: day1.Add(10 * time.Minute)})
	pnl, _ := sim.DailyPnL()
	if !pnl.Equal(decimal.NewFromInt(-1000)) {
		t.Errorf("session PnL after a 490 stop on 100 shares = %s, want -1000", pnl)
	}

	// A new date starts from zero.
	day2 := day1.Add(24 * time.Hour)
	order.Timestamp = day2
	sim.PlaceOrder(order)
	sim.CheckExits(models.Candle{Symbol: "SBIN", Open: decimal.NewFromInt(495), High: decimal.NewFromInt(495),
		Low: decimal.NewFromInt(480), Close: decimal.NewFromInt(485), StartTime: day2.Add(5 * time.Minute), EndTime: day2.Add(10 * time.Minute)})
	pnl, _ = sim.DailyPnL()
	if !pnl.Equal(decimal.NewFromInt(-1000)) {
		t.Errorf("session PnL on day 2 = %s, want -1000 (reset, not cumulative)", pnl)
	}
}
