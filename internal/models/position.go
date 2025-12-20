package models

import "github.com/shopspring/decimal"

type Position struct {
	Tradingsymbol string          `json:"tradingsymbol"`
	Exchange      string          `json:"exchange"`
	Product       string          `json:"product"` // MIS, CNC, NRML
	Quantity      int             `json:"quantity"`
	AveragePrice  decimal.Decimal `json:"average_price"`
	NetQuantity   int             `json:"net_quantity"` // Net open quantity
	LastPrice     decimal.Decimal `json:"last_price"`
	PnL           decimal.Decimal `json:"pnl"`
}
