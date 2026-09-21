// Package costs is the one place Zerodha's charge sheet lives.
//
// cmd/optbt prices a backtest with it and the paper broker debits every fill
// with it, so a paper run and the backtest it is being compared against are
// costed by the same arithmetic. Rates are verified against worked examples
// from Zerodha's own brokerage calculator in costs_test.go rather than
// transcribed from a rate card — an out-of-date statutory rate is invisible
// in a backtest and shifts every net figure.
//
// Bid-ask is not a charge and is not here: it is a fill-price effect, and the
// paper broker models it as such (see PaperAdapter's spread option).
package costs

import (
	"regexp"
	"strconv"
	"strings"
)

// optionSymbol matches a derivative trading symbol such as NIFTY26SEP24000CE:
// a strike (digits) directly before the CE/PE suffix. A bare "CE" suffix is
// not enough — RELIANCE ends in one.
var optionSymbol = regexp.MustCompile(`\d(CE|PE)$`)

// Sheet is one segment's charge schedule. Percentages are in percent, so 0.15
// means 0.15% of turnover.
type Sheet struct {
	// BrokeragePerOrder is the flat rupee charge per executed order, capped
	// by BrokerageMaxPct of turnover when that is set (equity intraday is
	// "0.03% or Rs20, whichever is lower").
	BrokeragePerOrder float64
	BrokerageMaxPct   float64
	STTBuyPct         float64 // STT on the buy leg (delivery only)
	STTSellPct        float64 // STT on the sell leg
	TxnPct            float64 // exchange transaction charge, both legs
	StampPct          float64 // stamp duty, buy leg only
	SEBIPct           float64 // SEBI turnover fee, both legs
	GSTPct            float64 // on brokerage + transaction charge + SEBI fee
	DPChargeOnSell    float64 // rupees per sell order (delivery only)
}

// Options is the NSE/BSE index and equity option schedule, on premium
// turnover. Pinned by TestOptionsSheetMatchesZerodhaCalculator.
func Options() Sheet {
	return Sheet{
		BrokeragePerOrder: 20,
		STTSellPct:        0.15,
		TxnPct:            0.03553,
		StampPct:          0.0025,
		SEBIPct:           0.0001,
		GSTPct:            18,
	}
}

// EquityIntraday is the NSE cash MIS schedule. Round trip ~6 bps of notional,
// which is the -cost-bps 3 the backtester has always used for ORB.
func EquityIntraday() Sheet {
	return Sheet{
		BrokeragePerOrder: 20,
		BrokerageMaxPct:   0.03,
		STTSellPct:        0.025,
		TxnPct:            0.00297,
		StampPct:          0.003,
		SEBIPct:           0.0001,
		GSTPct:            18,
	}
}

// EquityDelivery is the NSE cash CNC schedule: no brokerage, STT on both
// legs, a DP charge on the sell. Round trip ~22 bps — the -cost-bps 11
// CLAUDE.md records for dailyrev.
func EquityDelivery() Sheet {
	return Sheet{
		STTBuyPct:      0.1,
		STTSellPct:     0.1,
		TxnPct:         0.00297,
		StampPct:       0.015,
		SEBIPct:        0.0001,
		GSTPct:         18,
		DPChargeOnSell: 15.34,
	}
}

// IsOptionSymbol reports whether a trading symbol is an option contract.
func IsOptionSymbol(symbol string) bool {
	return optionSymbol.MatchString(strings.ToUpper(symbol))
}

// ForInstrument picks the schedule for an order from what the order says
// about itself. Options are recognised by exchange (NFO/BFO) or by the
// strike+CE/PE suffix, so a contract routed without an exchange is still
// costed as one rather than as a Rs20 equity trade.
func ForInstrument(exchange, product, symbol string) Sheet {
	ex := strings.ToUpper(exchange)
	if ex == "NFO" || ex == "BFO" || IsOptionSymbol(symbol) {
		return Options()
	}
	if strings.ToUpper(product) == "CNC" {
		return EquityDelivery()
	}
	return EquityIntraday()
}

// Breakdown is one executed order's charge, itemised the way a contract
// note itemises it. Every field is rupees; Total is their sum and is what
// Leg returns.
type Breakdown struct {
	Brokerage float64 `json:"brokerage"`
	STT       float64 `json:"stt"`
	Txn       float64 `json:"txn"` // exchange transaction charge
	Stamp     float64 `json:"stamp"`
	SEBI      float64 `json:"sebi"`
	GST       float64 `json:"gst"`
	DP        float64 `json:"dp"` // depository charge, delivery sells only
}

// Total is the rupee sum of every component.
func (b Breakdown) Total() float64 {
	return b.Brokerage + b.STT + b.Txn + b.Stamp + b.SEBI + b.GST + b.DP
}

// Add returns the component-wise sum, for folding an entry and an exit leg
// into one round trip.
func (b Breakdown) Add(o Breakdown) Breakdown {
	return Breakdown{
		Brokerage: b.Brokerage + o.Brokerage, STT: b.STT + o.STT, Txn: b.Txn + o.Txn,
		Stamp: b.Stamp + o.Stamp, SEBI: b.SEBI + o.SEBI, GST: b.GST + o.GST, DP: b.DP + o.DP,
	}
}

// Scale returns every component multiplied by f, for apportioning a fill's
// charge across the lots it opened or closed.
func (b Breakdown) Scale(f float64) Breakdown {
	return Breakdown{
		Brokerage: b.Brokerage * f, STT: b.STT * f, Txn: b.Txn * f,
		Stamp: b.Stamp * f, SEBI: b.SEBI * f, GST: b.GST * f, DP: b.DP * f,
	}
}

// Metadata keys under which a fill's line items travel with the order, and
// the trade's after the tracker has folded both legs. Bid-ask is not a
// charge (see the package comment) but it is money the trade paid, so the
// dashboard files it under the same roof as CostSpread.
const (
	KeyBrokerage = "CostBrokerage"
	KeySTT       = "CostSTT"
	KeyTxn       = "CostTxn"
	KeyStamp     = "CostStamp"
	KeySEBI      = "CostSEBI"
	KeyGST       = "CostGST"
	KeyDP        = "CostDP"
	KeySpread    = "CostSpread"
)

// Items maps a breakdown onto its metadata keys.
func Items(b Breakdown) map[string]float64 {
	return map[string]float64{
		KeyBrokerage: b.Brokerage, KeySTT: b.STT, KeyTxn: b.Txn, KeyStamp: b.Stamp,
		KeySEBI: b.SEBI, KeyGST: b.GST, KeyDP: b.DP,
	}
}

// FromItems is the inverse of Items: unparseable or absent keys read as zero.
func FromItems(meta map[string]string) Breakdown {
	f := func(k string) float64 {
		v, _ := strconv.ParseFloat(meta[k], 64)
		return v
	}
	return Breakdown{
		Brokerage: f(KeyBrokerage), STT: f(KeySTT), Txn: f(KeyTxn), Stamp: f(KeyStamp),
		SEBI: f(KeySEBI), GST: f(KeyGST), DP: f(KeyDP),
	}
}

// LegBreakdown itemises the charge for one executed order of the given
// turnover (price x quantity). sell selects the sell-side statutory rates.
func (s Sheet) LegBreakdown(sell bool, turnover float64) Breakdown {
	b := Breakdown{Brokerage: s.BrokeragePerOrder}
	if s.BrokerageMaxPct > 0 {
		if capped := turnover * s.BrokerageMaxPct / 100; capped < b.Brokerage {
			b.Brokerage = capped
		}
	}
	b.STT = turnover * s.STTBuyPct / 100
	b.Stamp = turnover * s.StampPct / 100
	if sell {
		b.STT = turnover * s.STTSellPct / 100
		b.Stamp = 0
		b.DP = s.DPChargeOnSell
	}
	b.Txn = turnover * s.TxnPct / 100
	b.SEBI = turnover * s.SEBIPct / 100
	b.GST = (b.Brokerage + b.Txn + b.SEBI) * s.GSTPct / 100
	return b
}

// Leg returns the rupee charge for one executed order of the given turnover
// (price x quantity). sell selects the sell-side statutory rates.
func (s Sheet) Leg(sell bool, turnover float64) float64 {
	return s.LegBreakdown(sell, turnover).Total()
}

// RoundTrip is the charge for buying at buy and selling at sell, qty units.
func (s Sheet) RoundTrip(buy, sell float64, qty float64) float64 {
	return s.Leg(false, buy*qty) + s.Leg(true, sell*qty)
}
