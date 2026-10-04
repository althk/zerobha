package core

import (
	"testing"

	"zerobha/internal/models"
	"zerobha/internal/risk"
	"zerobha/pkg/broker"
	"zerobha/pkg/db"

	"github.com/shopspring/decimal"
)

// namedStrategy is a scripted strategy with its own name and, optionally, an
// exit it advises once.
type namedStrategy struct {
	*scriptedSignals
	name   string
	advice *ExitAdvice
}

func (n *namedStrategy) Name() string { return n.name }
func (n *namedStrategy) ExitAdvice(models.Candle) *ExitAdvice {
	a := n.advice
	n.advice = nil
	return a
}

func named(name string, sigs ...*models.Signal) *namedStrategy {
	return &namedStrategy{scriptedSignals: &scriptedSignals{signals: sigs}, name: name}
}

func optionLeg(symbol, underlying string) *models.Signal {
	return &models.Signal{Symbol: symbol, Type: models.BuySignal, Price: decimal.NewFromInt(100),
		StopLoss: decimal.NewFromInt(80), ProductType: "MIS", Exchange: "NFO",
		Metadata: map[string]string{"Strategy": "x", "Underlying": underlying}}
}

func multiEngine(t *testing.T, store *db.Store, strategies ...Strategy) (*Engine, *broker.PaperAdapter) {
	t.Helper()
	paper := broker.NewPaperAdapter(nil, decimal.NewFromInt(1000000), broker.WithoutPaperCharges())
	e := NewEngine(strategies[0], paper, risk.NewManager(nil, decimal.Zero, 100, 0), nil, nil, store)
	for _, s := range strategies {
		e.Runners = append(e.Runners, &Runner{Strategy: s, TradeCutoffMin: 24 * 60, MaxConcurrent: 5})
	}
	e.MaxConcurrent = 10
	return e, paper
}

// First come, first served: the earlier runner takes NIFTY, and the later
// one's NIFTY leg is refused on the same candle while its SENSEX leg is not.
func TestRunnersFirstComeFirstServedPerUnderlying(t *testing.T) {
	a := named("alpha", optionLeg("NIFTY26OCT24000CE", "NIFTY 50"))
	b := named("beta", optionLeg("NIFTY26OCT24500PE", "NIFTY 50"), optionLeg("SENSEX26OCT80000CE", "SENSEX"))
	e, paper := multiEngine(t, nil, a, b)

	e.Execute(limitBar(0)) // alpha opens the CE; beta's PE is refused
	if openQty(t, paper, "NIFTY26OCT24000CE") == 0 {
		t.Fatal("alpha should hold the NIFTY CE")
	}
	if q := openQty(t, paper, "NIFTY26OCT24500PE"); q != 0 {
		t.Errorf("beta's NIFTY PE should be refused while alpha holds NIFTY, qty %d", q)
	}
	e.Execute(limitBar(1)) // beta's SENSEX leg: a different underlying
	if openQty(t, paper, "SENSEX26OCT80000CE") == 0 {
		t.Error("beta's SENSEX leg should be allowed")
	}
}

// A strategy's exit advice may only close what it opened, and its own
// square-off leaves the other strategy's positions alone.
func TestRunnersCloseOnlyTheirOwnPositions(t *testing.T) {
	a := named("alpha", equitySignal("SBIN", 500, 490))
	b := named("beta", nil, equitySignal("TCS", 500, 490))
	e, paper := multiEngine(t, nil, a, b)
	e.Execute(limitBar(0)) // alpha: SBIN
	e.Execute(limitBar(1)) // beta: TCS

	b.advice = &ExitAdvice{Symbol: "SBIN", ForSide: models.BuySignal, Reason: "beta wants out"}
	e.Execute(limitBar(2))
	if openQty(t, paper, "SBIN") == 0 {
		t.Fatal("beta's advice must not close alpha's SBIN")
	}
	a.advice = &ExitAdvice{Symbol: "SBIN", ForSide: models.BuySignal, Reason: "alpha wants out"}
	e.Execute(limitBar(3))
	if q := openQty(t, paper, "SBIN"); q != 0 {
		t.Errorf("alpha's own advice should close SBIN, qty %d", q)
	}

	e.SquareOffStrategy("alpha")
	if openQty(t, paper, "TCS") == 0 {
		t.Error("alpha's square-off must not close beta's TCS")
	}
	e.SquareOffStrategy("beta")
	if q := openQty(t, paper, "TCS"); q != 0 {
		t.Errorf("beta's square-off should close TCS, qty %d", q)
	}
}

// Each runner is held to its own concurrency cap, not only the account's.
func TestRunnerConcurrencyCap(t *testing.T) {
	a := named("alpha", equitySignal("SBIN", 500, 490), equitySignal("TCS", 500, 490))
	b := named("beta", nil, nil, equitySignal("INFY", 500, 490))
	e, paper := multiEngine(t, nil, a, b)
	e.Runners[0].MaxConcurrent = 1
	for i := 0; i < 3; i++ {
		e.Execute(limitBar(i))
	}
	if openQty(t, paper, "SBIN") == 0 {
		t.Fatal("alpha's first position should open")
	}
	if q := openQty(t, paper, "TCS"); q != 0 {
		t.Errorf("alpha is capped at 1 position, but TCS opened with qty %d", q)
	}
	if openQty(t, paper, "INFY") == 0 {
		t.Error("beta has its own slots and should open INFY")
	}
}

// A runner sees only its own timeframe and watchlist.
func TestRunnerCandleRouting(t *testing.T) {
	r := &Runner{Timeframe: "5m0s", Symbols: map[string]bool{"SBIN": true}}
	for _, tc := range []struct {
		sym, tf string
		want    bool
	}{{"SBIN", "5m0s", true}, {"SBIN", "15m0s", false}, {"TCS", "5m0s", false}} {
		if got := r.wants(models.Candle{Symbol: tc.sym, Timeframe: tc.tf}); got != tc.want {
			t.Errorf("wants(%s, %s) = %v, want %v", tc.sym, tc.tf, got, tc.want)
		}
	}
	if !(&Runner{}).wants(models.Candle{Symbol: "ANY", Timeframe: "1h0m0s"}) {
		t.Error("an unfiltered runner should take every candle")
	}
}

// After a restart the engine has no memory of who opened what; the order
// journal's Owner tag must answer, or the second strategy would trade into
// the first one's underlying.
func TestOwnershipSurvivesARestart(t *testing.T) {
	store, err := db.NewStore(t.TempDir() + "/owners.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_ = store.SaveOrder(models.Order{ID: "1", Symbol: "NIFTY26OCT24000CE",
		Metadata: map[string]string{"Owner": "alpha", "Underlying": "NIFTY 50"}}, "SUBMITTED")

	e, _ := multiEngine(t, store, named("alpha"), named("beta"))
	held := []models.Position{{Tradingsymbol: "NIFTY26OCT24000CE", NetQuantity: 65}}
	if got := e.underlyingHeldByOther("NIFTY 50", "beta", held); got != "alpha" {
		t.Errorf("held by %q, want alpha (from the journal)", got)
	}
	if got := e.underlyingHeldByOther("NIFTY 50", "alpha", held); got != "" {
		t.Errorf("alpha's own position reported as held by %q", got)
	}
	untracked := []models.Position{{Tradingsymbol: "BANKNIFTY26OCT52000CE", NetQuantity: 30, Underlying: "NIFTY BANK"}}
	if got := e.underlyingHeldByOther("NIFTY BANK", "alpha", untracked); got == "" {
		t.Error("a position no strategy opened must block entries on its underlying")
	}
}

func TestSingleRunnerModeKeepsLegacyFields(t *testing.T) {
	e := NewEngine(named("solo"), nil, risk.NewManager(nil, decimal.Zero, 1, 0), nil, nil, nil)
	e.TradeCutoffMin, e.UptrendOnly, e.MaxConcurrent = 600, false, 3
	rs := e.runners()
	if len(rs) != 1 || rs[0].TradeCutoffMin != 600 || rs[0].UptrendOnly || rs[0].MaxConcurrent != 3 || rs[0].Name() != "solo" {
		t.Errorf("implicit runner = %+v, want the engine's own settings", rs[0])
	}
}
