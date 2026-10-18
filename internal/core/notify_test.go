package core

import (
	"strings"
	"sync"
	"testing"
	"time"

	"zerobha/internal/models"
	"zerobha/internal/risk"
	"zerobha/pkg/broker"
	"zerobha/pkg/notify"

	"github.com/shopspring/decimal"
)

type recordingNotifier struct {
	mu   sync.Mutex
	msgs []string
	lvls []notify.Level
}

func (r *recordingNotifier) Notify(l notify.Level, m string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, m)
	r.lvls = append(r.lvls, l)
}

func (r *recordingNotifier) count(substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, m := range r.msgs {
		if strings.Contains(m, substr) {
			n++
		}
	}
	return n
}

// An entry is announced, and a tripped kill switch alerts once a day rather
// than on every signal it refuses.
func TestEngineNotifiesEntryAndRiskBlockOncePerDay(t *testing.T) {
	paper := broker.NewPaperAdapter(nil, decimal.NewFromInt(1000000), broker.WithoutPaperCharges())
	strat := &signalEveryBar{sig: models.Signal{
		Symbol: "SBIN", Type: models.BuySignal, Price: decimal.NewFromInt(500),
		StopLoss: decimal.NewFromInt(490), ProductType: "MIS", Exchange: "NSE",
		Metadata: map[string]string{"Strategy": "always"},
	}}
	e := NewEngine(strat, paper, risk.NewManager(nil, decimal.NewFromInt(5000), 100, 0), nil, nil, nil)
	e.UptrendOnly = false
	e.MaxCapitalPerTrade = 500000
	e.PaperMode = true
	rec := &recordingNotifier{}
	e.Notifier = rec

	start := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	bar := func(i int) models.Candle {
		st := start.Add(time.Duration(i) * 5 * time.Minute)
		return models.Candle{Symbol: "SBIN", Close: decimal.NewFromInt(500), StartTime: st, EndTime: st.Add(5 * time.Minute)}
	}

	e.Execute(bar(1))
	if rec.count("[PAPER] 🟢 ENTRY always: BUY SBIN") != 1 {
		t.Fatalf("want one paper entry notification, got %q", rec.msgs)
	}

	// Stop out past the daily limit, then two more signals get refused.
	paper.OnTick("SBIN", decimal.NewFromInt(440), start.Add(6*time.Minute))
	e.Execute(bar(2))
	e.Execute(bar(3))
	if n := rec.count("Risk limit hit"); n != 1 {
		t.Fatalf("want one risk alert for the day, got %d: %q", n, rec.msgs)
	}
}

func TestEngineWithoutNotifierDoesNotPanic(t *testing.T) {
	e := &Engine{}
	e.notify(notify.Alert, "x")
}
