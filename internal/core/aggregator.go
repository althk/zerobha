package core

import (
	"time"

	"zerobha/internal/models"
	"zerobha/pkg/nseutils"
)

// CandleBuilder aggregates a stream of ticks into OHLC candles.
// It maintains the state of the currently forming candle.
type CandleBuilder struct {
	current   *models.Candle
	interval  time.Duration
	completed chan<- models.Candle
}

// NewCandleBuilder initializes a builder for a specific timeframe.
// It returns a pointer to the builder and the channel where completed candles will be sent.
func NewCandleBuilder(interval time.Duration, out chan<- models.Candle) *CandleBuilder {
	return &CandleBuilder{
		interval:  interval,
		completed: out,
	}
}

// Update ingests a new tick and updates the current candle state.
// If the tick belongs to a new time bucket, the previous candle is finalized and emitted.
func (b *CandleBuilder) Update(tick models.Tick) {
	// 1. Determine which time bucket this tick belongs to (Anchor to NSE Open)
	bucketStart := nseutils.CandleStartTime(tick.Timestamp, b.interval)

	// 2. Check if we need to close the previous candle
	if b.current != nil && !b.current.StartTime.Equal(bucketStart) {
		b.closeCurrent(bucketStart)
	}

	// 3. Initialize new candle if necessary
	if b.current == nil {
		b.current = &models.Candle{
			Symbol:    tick.Symbol,
			Timeframe: b.interval.String(),
			Open:      tick.Price,
			High:      tick.Price,
			Low:       tick.Price,
			Close:     tick.Price,
			Volume:    tick.Volume,
			StartTime: bucketStart,
		}
		return
	}

	// 4. Update the existing candle logic
	b.merge(tick)
}

// Flush forces the closure of any pending candle.
// This should be called on market close or shutdown.
func (b *CandleBuilder) Flush() {
	if b.current != nil {
		// In a flush scenario, the EndTime is the current moment
		b.closeCurrent(time.Now())
	}
}

// closeCurrent finalizes the candle and resets state.
func (b *CandleBuilder) closeCurrent(endTime time.Time) {
	b.current.IsComplete = true
	b.current.EndTime = endTime

	// Emit the completed candle
	// Note: This is a blocking send. In a high-throughput system, ensure the consumer is fast.
	b.completed <- *b.current

	b.current = nil
}

// merge updates the high/low/close/volume of the current candle.
func (b *CandleBuilder) merge(tick models.Tick) {
	if tick.Price.GreaterThan(b.current.High) {
		b.current.High = tick.Price
	}
	if tick.Price.LessThan(b.current.Low) {
		b.current.Low = tick.Price
	}
	b.current.Close = tick.Price
	b.current.Volume = b.current.Volume.Add(tick.Volume)
}
