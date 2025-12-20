package models

import (
	"time"

	"github.com/shopspring/decimal"
)

type OrderStatus string

const (
	OrderPending OrderStatus = "PENDING"
	OrderFilled  OrderStatus = "FILLED"
	OrderFailed  OrderStatus = "FAILED"
	OrderClosed  OrderStatus = "CLOSED"
)

type Order struct {
	ID          string            `json:"id"`
	Symbol      string            `json:"symbol"`
	Side        SignalType        `json:"side"`         // Reusing SignalType (Buy/Sell)
	Type        string            `json:"type"`         // MARKET, LIMIT, SL-M
	ProductType string            `json:"product_type"` // MIS, CNC
	Quantity    decimal.Decimal   `json:"quantity"`
	Price       decimal.Decimal   `json:"price"` // Limit price
	Timestamp   time.Time         `json:"timestamp"`
	Status      OrderStatus       `json:"status"`
	StopLoss    decimal.Decimal   `json:"stop_loss"`
	Target      decimal.Decimal   `json:"target"`
	ParentID    string            `json:"parent_id"` // Links the Exit order to the Entry order
	Metadata    map[string]string `json:"metadata"`
}
