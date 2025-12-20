// Package nseutils provides utilities for NSE market time calculations.
package nseutils

import (
	"time"
)

var istLocation *time.Location

func init() {
	var err error
	// standard IANA Time Zone database name for India
	istLocation, err = time.LoadLocation("Asia/Kolkata")
	if err != nil {
		// In a production trading system, failing to load the correct timezone
		// is a fatal error. We should not proceed.
		panic("CRITICAL: Could not load Asia/Kolkata timezone. Ensure tzdata is installed. Error: " + err.Error())
	}
}

// CandleStartTime aligns a tick timestamp to the market open (09:15 IST)
// strictly based on the provided interval.
func CandleStartTime(tickTime time.Time, interval time.Duration) time.Time {
	// 1. Convert tick to IST
	t := tickTime.In(istLocation)

	// 2. Define Market Open for THIS specific day (09:15:00)
	marketOpen := time.Date(t.Year(), t.Month(), t.Day(), 9, 15, 0, 0, istLocation)

	// 3. Handle Pre-market ticks (before 09:15)
	// We clamp them to market open so they are included in the first candle
	if t.Before(marketOpen) {
		return marketOpen
	}

	// 4. Calculate duration since market open
	sinceOpen := t.Sub(marketOpen)

	// 5. Calculate the bucket start
	// Example: 09:20, 15m interval. sinceOpen = 5m.
	// remainder = 5m. offset = 0. result = 09:15.
	remainder := sinceOpen % interval
	offset := sinceOpen - remainder

	return marketOpen.Add(offset)
}
