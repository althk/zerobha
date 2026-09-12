package strategy

import (
	"testing"
	"time"

	"zerobha/internal/config"
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

func makeCandle(sym string, t time.Time, o, h, l, c float64) models.Candle {
	return models.Candle{
		Symbol:    sym,
		StartTime: t,
		EndTime:   t.Add(time.Minute),
		Open:      decimal.NewFromFloat(o),
		High:      decimal.NewFromFloat(h),
		Low:       decimal.NewFromFloat(l),
		Close:     decimal.NewFromFloat(c),
		Volume:    decimal.NewFromInt(1000),
	}
}

func TestEMACross_GoldenCrossAndExit(t *testing.T) {
	cfg := config.DefaultEMACrossConfig()
	cfg.FastPeriod = 3
	cfg.SlowPeriod = 5
	cfg.ATRPeriod = 3
	cfg.SLATRMult = 2.0
	cfg.TPATRMult = 5.0
	cfg.ProductType = "MIS"
	cfg.EntryStartMin = 0
	cfg.EntryCutoffMin = 0

	s := NewEMACrossStrategy([]string{"TEST"}, cfg)

	startTime, _ := time.Parse("2006-01-02 15:04", "2026-08-01 09:15")

	// Phase 1: Flat/downward prices to establish fast <= slow
	prices := []float64{100, 99, 98, 97, 96, 95}
	for i, p := range prices {
		c := makeCandle("TEST", startTime.Add(time.Duration(i)*time.Minute), p, p+1, p-1, p)
		_ = s.ExitAdvice(c)
		sig := s.OnCandle(c)
		if sig != nil {
			t.Fatalf("unexpected signal during warmup bar %d", i)
		}
	}

	// Phase 2: Strong upward thrust to trigger golden cross (fast > slow)
	upPrices := []float64{105, 115, 125}
	var buySignal *models.Signal
	for i, p := range upPrices {
		idx := len(prices) + i
		c := makeCandle("TEST", startTime.Add(time.Duration(idx)*time.Minute), p-2, p+2, p-3, p)
		_ = s.ExitAdvice(c)
		if sig := s.OnCandle(c); sig != nil {
			if sig.Type == models.BuySignal {
				buySignal = sig
				break
			}
		}
	}

	if buySignal == nil {
		t.Fatal("expected BuySignal on golden cross, got nil")
	}

	if buySignal.ProductType != "MIS" {
		t.Errorf("expected product type MIS, got %s", buySignal.ProductType)
	}

	if !buySignal.StopLoss.LessThan(buySignal.Price) {
		t.Errorf("expected StopLoss < Price, got SL=%s, Price=%s", buySignal.StopLoss, buySignal.Price)
	}
	if !buySignal.Target.GreaterThan(buySignal.Price) {
		t.Errorf("expected Target > Price, got Target=%s, Price=%s", buySignal.Target, buySignal.Price)
	}

	// Phase 3: Now price drops sharply to trigger bearish cross and ExitAdvice
	downPrices := []float64{100, 80, 60}
	var exitAdvised bool
	for i, p := range downPrices {
		idx := len(prices) + len(upPrices) + i
		c := makeCandle("TEST", startTime.Add(time.Duration(idx)*time.Minute), p+2, p+3, p-2, p)
		advice := s.ExitAdvice(c)
		if advice != nil {
			if advice.ForSide != models.BuySignal {
				t.Errorf("expected ExitAdvice ForSide BuySignal, got %v", advice.ForSide)
			}
			exitAdvised = true
			break
		}
		_ = s.OnCandle(c)
	}

	if !exitAdvised {
		t.Fatal("expected ExitAdvice when fast EMA fell below slow EMA")
	}
}

func TestEMACross_CNCSuppressesShort(t *testing.T) {
	cfg := config.DefaultEMACrossConfig()
	cfg.FastPeriod = 3
	cfg.SlowPeriod = 5
	cfg.ATRPeriod = 3
	cfg.ProductType = "CNC"
	cfg.AssetType = "stocks"
	cfg.EntryStartMin = 0
	cfg.EntryCutoffMin = 0

	s := NewEMACrossStrategy([]string{"TEST"}, cfg)

	startTime, _ := time.Parse("2006-01-02 15:04", "2026-08-01 09:15")

	// Upward prices
	prices := []float64{100, 102, 104, 106, 108, 110}
	for i, p := range prices {
		c := makeCandle("TEST", startTime.Add(time.Duration(i)*time.Minute), p, p+1, p-1, p)
		_ = s.ExitAdvice(c)
		_ = s.OnCandle(c)
	}

	// Sharp downward prices to trigger death cross
	downPrices := []float64{95, 85, 75}
	for i, p := range downPrices {
		idx := len(prices) + i
		c := makeCandle("TEST", startTime.Add(time.Duration(idx)*time.Minute), p+2, p+3, p-2, p)
		_ = s.ExitAdvice(c)
		sig := s.OnCandle(c)
		if sig != nil && sig.Type == models.SellSignal {
			t.Fatalf("CNC mode on stocks must NOT emit SellSignal, got %v", sig)
		}
	}
}

func TestEMACross_DisabledTakeProfit(t *testing.T) {
	cfg := config.DefaultEMACrossConfig()
	cfg.FastPeriod = 3
	cfg.SlowPeriod = 5
	cfg.ATRPeriod = 3
	cfg.SLATRMult = 2.0
	cfg.TPATRMult = -1.0 // configured as -1 (< 0) -> no target
	cfg.ProductType = "MIS"
	cfg.EntryStartMin = 0
	cfg.EntryCutoffMin = 0

	s := NewEMACrossStrategy([]string{"TEST"}, cfg)
	startTime, _ := time.Parse("2006-01-02 15:04", "2026-08-01 09:15")

	prices := []float64{100, 99, 98, 97, 96, 95}
	for i, p := range prices {
		c := makeCandle("TEST", startTime.Add(time.Duration(i)*time.Minute), p, p+1, p-1, p)
		_ = s.ExitAdvice(c)
		_ = s.OnCandle(c)
	}

	upPrices := []float64{105, 115, 125}
	for i, p := range upPrices {
		idx := len(prices) + i
		c := makeCandle("TEST", startTime.Add(time.Duration(idx)*time.Minute), p-2, p+2, p-3, p)
		_ = s.ExitAdvice(c)
		sig := s.OnCandle(c)
		if sig != nil && sig.Type == models.BuySignal {
			if !sig.Target.IsZero() {
				t.Fatalf("expected Target to be zero when TPATRMult <= 0, got %s", sig.Target)
			}
			if !sig.StopLoss.IsPositive() {
				t.Fatalf("expected valid positive StopLoss, got %s", sig.StopLoss)
			}
			return
		}
	}
	t.Fatal("expected a buy signal during golden cross")
}

func BenchmarkEMACross(b *testing.B) {
	cfg := config.DefaultEMACrossConfig()
	s := NewEMACrossStrategy([]string{"TEST"}, cfg)
	t0 := time.Date(2026, 6, 1, 9, 15, 0, 0, time.UTC)
	c := makeCandle("TEST", t0, 100, 105, 95, 102)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.StartTime = t0.Add(time.Duration(i) * time.Minute)
		c.EndTime = c.StartTime.Add(time.Minute)
		s.ExitAdvice(c)
		s.OnCandle(c)
	}
}
