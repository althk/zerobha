package broker

import (
	"testing"
	"time"

	"zerobha/internal/models"
	"zerobha/pkg/costs"

	"github.com/shopspring/decimal"
)

// A paper fill that ignores charges reports gross PnL, and for an index
// weekly the ~80 bps between gross and net is the whole verdict. Every fill
// is debited with the same sheet cmd/optbt prices a backtest with.
func TestPaperFillsAreChargedWithTheSharedCostSheet(t *testing.T) {
	p := NewPaperAdapter(nil, rs(1000000))

	buy, err := p.PlaceOrder(entry("NIFTY26AUG24200CE", models.BuySignal, 650, 400))
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	wantBuy := costs.Options().Leg(false, 400*650)
	gotBuy, _ := decimal.RequireFromString(buy.Metadata["Costs"]).Float64()
	if abs64(gotBuy-wantBuy) > 0.01 {
		t.Errorf("buy charge = %.2f, sheet says %.2f", gotBuy, wantBuy)
	}
	bal, _ := p.GetBalance()
	// Premium is fully paid (not MIS-levered without a leverage map), plus the charge.
	wantBal := decimal.NewFromInt(1000000 - 400*650).Sub(decimal.NewFromFloat(wantBuy)).Round(2)
	if !bal.Equal(wantBal) {
		t.Errorf("balance after buy = %s, want %s (premium + charge)", bal, wantBal)
	}

	closed, err := p.ClosePosition("NIFTY26AUG24200CE", models.BuySignal, rs(450), time.Now(), "trail")
	if err != nil || !closed {
		t.Fatalf("close: closed=%v err=%v", closed, err)
	}
	wantSell := costs.Options().Leg(true, 450*650)
	positions, _ := p.GetPositions()
	if len(positions) != 1 {
		t.Fatalf("want 1 closed position row, got %d", len(positions))
	}
	gross := 50.0 * 650
	wantNet := gross - wantBuy - wantSell
	gotNet, _ := positions[0].PnL.Float64()
	if abs64(gotNet-wantNet) > 0.05 {
		t.Errorf("closed position PnL = %.2f, want gross %.0f less charges %.2f = %.2f",
			gotNet, gross, wantBuy+wantSell, wantNet)
	}
}

// The option spread is a fill-price effect: a buy fills above the last trade
// and a sell below, by half the configured full spread. Equities are not
// touched.
func TestPaperOptionSpreadMovesFillsAgainstTheTrader(t *testing.T) {
	p := NewPaperAdapter(nil, rs(1000000), WithoutPaperCharges(), WithPaperOptionSpread(20))

	buy, err := p.PlaceOrder(entry("NIFTY26AUG24200CE", models.BuySignal, 65, 200))
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	// 20 ticks x 0.05 = Rs1 full spread, half = 0.50 each way.
	if !buy.Price.Equal(decimal.NewFromFloat(200.5)) {
		t.Errorf("buy filled at %s, want 200.50 (last + half spread)", buy.Price)
	}
	if buy.Metadata["SpreadCost"] != "32.50" {
		t.Errorf("spread cost = %q, want 32.50 (0.50 x 65)", buy.Metadata["SpreadCost"])
	}
	p.ClosePosition("NIFTY26AUG24200CE", models.BuySignal, rs(210), time.Now(), "x")
	fills, _ := p.GetTrades()
	exit := fills[len(fills)-1]
	if !exit.Price.Equal(decimal.NewFromFloat(209.5)) {
		t.Errorf("sell filled at %s, want 209.50 (last - half spread)", exit.Price)
	}

	eq := entry("SBIN", models.BuySignal, 100, 500)
	eq.Exchange = "NSE"
	got, _ := p.PlaceOrder(eq)
	if !got.Price.Equal(rs(500)) {
		t.Errorf("equity fill = %s, spread must not apply to equities", got.Price)
	}
}

// The exit fill has to be pairable with the view that opened it: the entry
// order id, and the underlying's last price for the index-bps accounting.
func TestPaperExitCarriesEntryContextAndUnderlyingPrice(t *testing.T) {
	p := NewPaperAdapter(nil, rs(1000000), WithoutPaperCharges())

	e := entry("NIFTY26AUG24200CE", models.BuySignal, 65, 200)
	e.Metadata["Underlying"] = "NIFTY 50"
	e.Metadata["IndexEntry"] = "24300.00"
	buy, _ := p.PlaceOrder(e)

	// Index ticks arrive on the feed whether or not it is held.
	p.OnTick("NIFTY 50", decimal.NewFromFloat(24350.25), time.Now())
	p.OnTick("NIFTY26AUG24200CE", rs(230), time.Now())

	p.ClosePosition("NIFTY26AUG24200CE", models.BuySignal, rs(230), time.Now(), "EMACross index stop")
	fills, _ := p.GetTrades()
	exit := fills[len(fills)-1]
	if exit.Metadata["EntryOrderID"] != buy.ID {
		t.Errorf("exit EntryOrderID = %q, want %q", exit.Metadata["EntryOrderID"], buy.ID)
	}
	if exit.Metadata["UnderlyingLast"] != "24350.25" {
		t.Errorf("exit UnderlyingLast = %q, want 24350.25", exit.Metadata["UnderlyingLast"])
	}
	if exit.Metadata["Reason"] != "EMACross index stop" {
		t.Errorf("exit reason = %q", exit.Metadata["Reason"])
	}
	positions, _ := p.GetPositions()
	if positions[0].Underlying != "" {
		t.Errorf("closed position should not keep an underlying, got %q", positions[0].Underlying)
	}
}

func abs64(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
