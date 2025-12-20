package models

import (
	"time"

	"github.com/shopspring/decimal"
)

type Candle struct {
	Symbol     string          `json:"symbol"`
	Timeframe  string          `json:"timeframe"` // e.g., "15m", "1h", "1d"
	Open       decimal.Decimal `json:"open"`
	High       decimal.Decimal `json:"high"`
	Low        decimal.Decimal `json:"low"`
	Close      decimal.Decimal `json:"close"`
	Volume     decimal.Decimal `json:"volume"`
	StartTime  time.Time       `json:"start_time"`
	EndTime    time.Time       `json:"end_time"`
	IsComplete bool            `json:"is_complete"` // True when the time period has finished
}
