package models

import "github.com/shopspring/decimal"

type GTT struct {
	ID            int          `json:"id"` // Trigger ID
	Tradingsymbol string       `json:"tradingsymbol"`
	Exchange      string       `json:"exchange"`
	Type          string       `json:"type"`      // single, two-leg
	Product       string       `json:"product"`   // MIS, CNC
	Status        string       `json:"status"`    // active, triggered, etc.
	Condition     GTTCondition `json:"condition"` // Usually more complex in Kite, simplifying
	Orders        []GTTOrder   `json:"orders"`
}

type GTTCondition struct {
	Exchange      string          `json:"exchange"`
	Tradingsymbol string          `json:"tradingsymbol"`
	LastPrice     decimal.Decimal `json:"last_price"`
	TriggerValues []float64       `json:"trigger_values"`
}

type GTTOrder struct {
	TransactionType string          `json:"transaction_type"`
	Quantity        int             `json:"quantity"`
	Price           decimal.Decimal `json:"price"`
	Product         string          `json:"product"`
}
