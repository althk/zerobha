package models

import (
	"time"

	"github.com/shopspring/decimal"
)

type Tick struct {
	Symbol    string          `json:"symbol"`
	Price     decimal.Decimal `json:"price"`
	Volume    decimal.Decimal `json:"volume"` // Volume traded in this specific tick/packet
	Timestamp time.Time       `json:"timestamp"`
}
