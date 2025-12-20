package indicators

import "github.com/shopspring/decimal"

// SMA represents a Streaming Simple Moving Average.
// It uses a Ring Buffer to achieve O(1) updates.
type SMA struct {
	period int
	window []decimal.Decimal // Stores the last N values
	sum    decimal.Decimal
	index  int  // Current position in the ring buffer
	filled bool // Have we filled the window yet?
}

func NewSMA(period int) *SMA {
	return &SMA{
		period: period,
		window: make([]decimal.Decimal, period), // Pre-allocate fixed size
		sum:    decimal.Zero,
	}
}

func (s *SMA) Update(val decimal.Decimal) decimal.Decimal {
	// 1. Remove the old value from the Sum
	// (If window isn't full, this subtracts Zero, which is fine)
	oldest := s.window[s.index]
	s.sum = s.sum.Sub(oldest)

	// 2. Add the new value
	s.sum = s.sum.Add(val)
	s.window[s.index] = val

	// 3. Move the pointer
	s.index++
	if s.index >= s.period {
		s.index = 0
		s.filled = true
	}

	// 4. Calculate Average
	// If not full, we divide by the count of items seen so far (optional, or return 0)
	count := s.period
	if !s.filled {
		count = s.index
		if count == 0 {
			return val
		}
	}

	// Sum / Count
	return s.sum.Div(decimal.NewFromInt(int64(count)))
}

func (s *SMA) Value() decimal.Decimal {
	count := s.period
	if !s.filled {
		count = s.index
		if count == 0 {
			return decimal.Zero
		}
	}
	return s.sum.Div(decimal.NewFromInt(int64(count)))
}
