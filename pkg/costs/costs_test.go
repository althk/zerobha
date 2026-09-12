package costs

import (
	"math"
	"testing"
)

// Pinned to a worked example from Zerodha's brokerage calculator, the same one
// cmd/optbt pins, so the paper broker and the backtest cannot drift apart:
//
//	equity options, buy 100, sell 110, qty 400 (turnover 84,000)
//	brokerage 40, STT 66, exchange txn 29.85, GST 12.59, SEBI 0.08,
//	stamp 1  ->  total 149.52
func TestOptionsSheetMatchesZerodhaCalculator(t *testing.T) {
	got := Options().RoundTrip(100, 110, 400)
	if math.Abs(got-149.52) > 1.0 {
		t.Errorf("options round trip = %.2f, Zerodha calculator says 149.52", got)
	}
}

// Equity intraday: buy 1000 sell 1010, qty 100 (turnover 201,000). Zerodha:
// brokerage 40 (0.03% of 100k = 30 < 20? no: 30 > 20 so 20 each), STT 25.25,
// txn 5.97, SEBI 0.20, stamp 3, GST 4.71 -> ~79.
func TestEquityIntradayIsAboutSixBps(t *testing.T) {
	got := EquityIntraday().RoundTrip(1000, 1010, 100)
	bps := got / (1000 * 100) * 1e4
	if bps < 5 || bps > 9 {
		t.Errorf("equity intraday round trip = %.2f (%.1f bps), expected ~6-8 bps", got, bps)
	}
	// The Rs20 cap only binds above Rs66,667 of turnover; a small order pays 0.03%.
	small := EquityIntraday().Leg(false, 10000)
	if small > 5 {
		t.Errorf("small intraday order charged %.2f, brokerage should be 0.03%% = 3 plus statutory", small)
	}
}

func TestEquityDeliveryIsAboutTwentyTwoBps(t *testing.T) {
	got := EquityDelivery().RoundTrip(1000, 1000, 100)
	bps := got / (1000 * 100) * 1e4
	if bps < 20 || bps > 26 {
		t.Errorf("delivery round trip = %.1f bps, expected ~22", bps)
	}
}

func TestForInstrumentRecognisesOptionsWithoutExchange(t *testing.T) {
	if ForInstrument("", "MIS", "NIFTY26SEP24000CE").STTSellPct != Options().STTSellPct {
		t.Error("a CE symbol with no exchange should be costed as an option")
	}
	if ForInstrument("BFO", "MIS", "X").STTSellPct != Options().STTSellPct {
		t.Error("BFO should be costed as an option")
	}
	// RELIANCE ends in "CE" and is not an option.
	if ForInstrument("NSE", "CNC", "RELIANCE").STTBuyPct == 0 {
		t.Error("CNC equity should pay buy-side STT")
	}
	if ForInstrument("NSE", "MIS", "RELIANCE").BrokerageMaxPct == 0 {
		t.Error("RELIANCE should be costed as equity, not as an option")
	}
	if ForInstrument("NSE", "MIS", "RELIANCE").STTBuyPct != 0 {
		t.Error("MIS equity should not pay buy-side STT")
	}
}
