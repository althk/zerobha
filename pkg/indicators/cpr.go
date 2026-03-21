package indicators

import (
	"time"
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

// CPR represents the Central Pivot Range indicator.
// It computes Pivot, TC (Top Central), BC (Bottom Central), and standard
// support/resistance levels (R1, S1, R2, S2) from the previous trading day's
// High, Low, Close. Levels are static for the entire trading day and only
// recalculate at the day boundary.
type CPR struct {
	loc *time.Location

	// Previous day's HLC — locked at day boundary, used to compute levels
	prevHigh  decimal.Decimal
	prevLow   decimal.Decimal
	prevClose decimal.Decimal

	// Current day's running HLC — accumulated candle-by-candle
	curHigh  decimal.Decimal
	curLow   decimal.Decimal
	curClose decimal.Decimal

	// Computed levels (recalculated once at each new day boundary)
	Pivot decimal.Decimal // (H + L + C) / 3
	TC    decimal.Decimal // Top Central = 2*Pivot - BC
	BC    decimal.Decimal // Bottom Central = (H + L) / 2
	R1    decimal.Decimal // 2*Pivot - PrevLow
	S1    decimal.Decimal // 2*Pivot - PrevHigh
	R2    decimal.Decimal // Pivot + (PrevHigh - PrevLow)
	S2    decimal.Decimal // Pivot - (PrevHigh - PrevLow)

	lastDate string
	ready    bool
}

// NewCPR creates a new CPR indicator. loc is the exchange timezone used to
// determine day boundaries (e.g. Asia/Kolkata for NSE). This is critical
// because CSV data may have UTC timestamps — a candle at 9:15 IST is 03:45
// UTC, and without conversion the date boundary would be wrong.
func NewCPR(loc *time.Location) *CPR {
	return &CPR{loc: loc}
}

// Update processes a new candle and recalculates CPR levels if a new trading
// day is detected.
func (c *CPR) Update(candle models.Candle) {
	// Convert to exchange timezone for correct day boundary
	t := candle.StartTime
	if c.loc != nil {
		t = t.In(c.loc)
	}
	currentDate := t.Format("2006-01-02")

	if c.lastDate != "" && currentDate != c.lastDate {
		// Day changed — rotate current day's HLC into previous
		c.prevHigh = c.curHigh
		c.prevLow = c.curLow
		c.prevClose = c.curClose

		// Compute CPR levels from previous day
		c.computeLevels()
		c.ready = true

		// Reset current day
		c.curHigh = candle.High
		c.curLow = candle.Low
		c.curClose = candle.Close
	} else if c.lastDate == "" {
		// Very first candle — initialise current day
		c.curHigh = candle.High
		c.curLow = candle.Low
		c.curClose = candle.Close
	} else {
		// Same day — expand running HLC
		if candle.High.GreaterThan(c.curHigh) {
			c.curHigh = candle.High
		}
		if candle.Low.LessThan(c.curLow) {
			c.curLow = candle.Low
		}
		c.curClose = candle.Close
	}

	c.lastDate = currentDate
}

func (c *CPR) computeLevels() {
	three := decimal.NewFromInt(3)
	two := decimal.NewFromInt(2)

	// Pivot = (H + L + C) / 3
	c.Pivot = c.prevHigh.Add(c.prevLow).Add(c.prevClose).Div(three)

	// BC = (H + L) / 2
	c.BC = c.prevHigh.Add(c.prevLow).Div(two)

	// TC = 2 * Pivot - BC
	c.TC = c.Pivot.Mul(two).Sub(c.BC)

	// Ensure TC >= BC (swap if needed — happens when Close is below midpoint)
	if c.TC.LessThan(c.BC) {
		c.TC, c.BC = c.BC, c.TC
	}

	// R1 = 2*Pivot - PrevLow
	c.R1 = c.Pivot.Mul(two).Sub(c.prevLow)

	// S1 = 2*Pivot - PrevHigh
	c.S1 = c.Pivot.Mul(two).Sub(c.prevHigh)

	// R2 = Pivot + (PrevHigh - PrevLow)
	rangeDiff := c.prevHigh.Sub(c.prevLow)
	c.R2 = c.Pivot.Add(rangeDiff)

	// S2 = Pivot - (PrevHigh - PrevLow)
	c.S2 = c.Pivot.Sub(rangeDiff)
}

// IsReady returns true once at least one full previous day's data has been
// processed (i.e., a day boundary has been crossed).
func (c *CPR) IsReady() bool {
	return c.ready
}

// Width returns the absolute width of the Central Pivot Range (TC - BC).
func (c *CPR) Width() decimal.Decimal {
	return c.TC.Sub(c.BC).Abs()
}
