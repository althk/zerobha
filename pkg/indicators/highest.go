package indicators

import (
	"github.com/shopspring/decimal"
)

// Highest represents a rolling maximum indicator
type Highest struct {
	period int
	values []decimal.Decimal
}

// NewHighest creates a new Highest indicator
func NewHighest(period int) *Highest {
	return &Highest{
		period: period,
		values: make([]decimal.Decimal, 0, period),
	}
}

// Update adds a new value and returns the current maximum
func (h *Highest) Update(val decimal.Decimal) decimal.Decimal {
	h.values = append(h.values, val)
	if len(h.values) > h.period {
		h.values = h.values[1:]
	}

	return h.Value()
}

// Value returns the current maximum value in the window
func (h *Highest) Value() decimal.Decimal {
	if len(h.values) == 0 {
		return decimal.Zero
	}

	maxVal := h.values[0]
	for _, v := range h.values {
		if v.GreaterThan(maxVal) {
			maxVal = v
		}
	}
	return maxVal
}

// IsReady returns true if we have enough data for the full period
func (h *Highest) IsReady() bool {
	return len(h.values) >= h.period
}
