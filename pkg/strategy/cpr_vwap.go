package strategy

import (
	"log"
	"time"
	"zerobha/internal/config"
	"zerobha/internal/core"
	"zerobha/internal/models"
	"zerobha/pkg/db"
	"zerobha/pkg/indicators"

	"github.com/shopspring/decimal"
)

type CPRVWAPState struct {
	Symbol    string
	CPR       *indicators.CPR
	Vwap      *indicators.VWAP
	EMA9      *indicators.EMA
	VolSma    *indicators.SMA // 20-period volume SMA
	Atr       *indicators.ATR
	Adx       *indicators.ADX
	Rsi       *indicators.RSI
	LastDate  string
	LastClose decimal.Decimal

	// VWAP ring buffer for slope detection (last 4 values)
	VwapHist  [4]decimal.Decimal
	VwapIdx   int
	VwapCount int

	// One signal per symbol per day
	SignaledToday bool
}

func NewCPRVWAPState(symbol string, loc *time.Location) *CPRVWAPState {
	return &CPRVWAPState{
		Symbol: symbol,
		CPR:    indicators.NewCPR(loc),
		Vwap:   indicators.NewVWAP(),
		EMA9:   indicators.NewEMA(9),
		VolSma: indicators.NewSMA(20),
		Atr:    indicators.NewATR(14),
		Adx:    indicators.NewADX(14),
		Rsi:    indicators.NewRSI(14),
	}
}

type CPRVWAPStrategy struct {
	symbols []string
	states  map[string]*CPRVWAPState
	db      *db.Store
	cfg     config.CPRVWAPConfig
	loc     *time.Location
}

func NewCPRVWAPStrategy(symbols []string, cfg config.CPRVWAPConfig) *CPRVWAPStrategy {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		loc = time.FixedZone("IST", 5*3600+1800)
	}
	return &CPRVWAPStrategy{
		symbols: symbols,
		states:  make(map[string]*CPRVWAPState),
		cfg:     cfg,
		loc:     loc,
	}
}

func (s *CPRVWAPStrategy) Name() string {
	return "CPRVWAP"
}

func (s *CPRVWAPStrategy) SetDB(store *db.Store) {
	s.db = store
}

func (s *CPRVWAPStrategy) SaveState(symbol string) {
	if s.db == nil {
		return
	}
	state, ok := s.states[symbol]
	if !ok {
		return
	}

	pState := map[string]interface{}{
		"LastDate":  state.LastDate,
		"LastClose": state.LastClose,
	}

	if err := s.db.SetState("CPRVWAP_"+symbol, pState); err != nil {
		log.Printf("ERROR: Failed to save CPRVWAP state for %s: %v", symbol, err)
	}
}

func (s *CPRVWAPStrategy) LoadState(symbol string) {
	if s.db == nil {
		return
	}
	var pState struct {
		LastDate  string
		LastClose decimal.Decimal
	}

	if err := s.db.GetState("CPRVWAP_"+symbol, &pState); err != nil {
		return
	}

	state, ok := s.states[symbol]
	if !ok {
		state = NewCPRVWAPState(symbol, s.loc)
		s.states[symbol] = state
	}

	state.LastDate = pState.LastDate
	state.LastClose = pState.LastClose
	log.Printf("LOADED STATE for %s: LastDate=%s LastClose=%s", symbol, state.LastDate, state.LastClose)
}

func (s *CPRVWAPStrategy) Init(provider core.DataProvider) error {
	log.Println("Initializing CPRVWAP Strategy...")

	for _, sym := range s.symbols {
		state := NewCPRVWAPState(sym, s.loc)
		s.states[sym] = state
		s.LoadState(sym)

		// Fetch 15 days of 5-minute history to warm up indicators (especially CPR)
		candles, err := provider.History(sym, "5minute", 15)
		if err != nil {
			log.Printf("WARNING: Failed to fetch history for %s: %v", sym, err)
			continue
		}

		for _, c := range candles {
			state.CPR.Update(c)
			state.Vwap.Update(c)
			state.EMA9.Update(c.Close)
			state.VolSma.Update(c.Volume)
			state.Atr.Update(c)
			state.Adx.Update(c)
			state.Rsi.Update(c.Close)
		}
	}
	return nil
}

func (s *CPRVWAPStrategy) OnCandle(candle models.Candle) *models.Signal {
	state, ok := s.states[candle.Symbol]
	if !ok {
		state = NewCPRVWAPState(candle.Symbol, s.loc)
		s.states[candle.Symbol] = state
	}

	// Always update LastClose when done, regardless of early returns
	defer func() { state.LastClose = candle.Close }()

	// 1. Day reset
	currentDate := candle.StartTime.In(s.loc).Format("2006-01-02")
	if state.LastDate != currentDate {
		state.LastDate = currentDate
		state.SignaledToday = false
		state.VwapCount = 0
		state.VwapIdx = 0
		s.SaveState(candle.Symbol)
	}

	// 2. Update indicators
	state.CPR.Update(candle)
	currentVwap := state.Vwap.Update(candle)
	ema9Val := state.EMA9.Update(candle.Close)
	prevAvgVol := state.VolSma.Value()
	state.VolSma.Update(candle.Volume)
	atrVal := state.Atr.Update(candle)
	prevAdx := state.Adx.Value()
	adxVal := state.Adx.Update(candle)
	rsiVal := state.Rsi.Update(candle.Close)

	// 3. Store VWAP in ring buffer for slope
	state.VwapHist[state.VwapIdx%4] = currentVwap
	state.VwapIdx++
	if state.VwapCount < 4 {
		state.VwapCount++
	}

	// 4. Guards
	if !state.CPR.IsReady() {
		return nil
	}
	if state.SignaledToday {
		return nil
	}
	if atrVal.IsZero() {
		return nil
	}

	// 5. Time window filter (IST)
	istTime := candle.StartTime.In(s.loc)
	h, m, _ := istTime.Clock()
	timeInMinutes := h*60 + m
	if timeInMinutes < s.cfg.EntryWindowStart || timeInMinutes >= s.cfg.EntryWindowEnd {
		return nil
	}

	// 6. CPR width filter
	cprWidth := state.CPR.Width()
	closePrice := candle.Close
	if closePrice.IsZero() {
		return nil
	}
	hundred := decimal.NewFromInt(100)
	widthPct, _ := cprWidth.Div(closePrice).Mul(hundred).Float64()
	if widthPct < s.cfg.MinCPRWidthPct || widthPct > s.cfg.MaxCPRWidthPct {
		return nil
	}

	// 7. Common filters: volume and ADX
	volume := candle.Volume
	volumeOK := volume.GreaterThan(prevAvgVol)
	adxOK := adxVal.GreaterThan(decimal.NewFromFloat(s.cfg.ADXThreshold)) && adxVal.GreaterThan(prevAdx)
	if !volumeOK || !adxOK {
		return nil
	}

	// 8. EMA proximity check — |Close - EMA9| / Close < MaxEMADistPct%
	emaDist := closePrice.Sub(ema9Val).Abs()
	emaDistPct, _ := emaDist.Div(closePrice).Mul(hundred).Float64()
	if emaDistPct > s.cfg.MaxEMADistPct {
		return nil
	}

	// 9. VWAP slope
	vwapSlope := 0 // neutral
	if state.VwapCount >= 4 {
		oldIdx := (state.VwapIdx - 4) % 4
		if oldIdx < 0 {
			oldIdx += 4
		}
		oldVwap := state.VwapHist[oldIdx]
		if currentVwap.GreaterThan(oldVwap) {
			vwapSlope = 1 // rising
		} else if currentVwap.LessThan(oldVwap) {
			vwapSlope = -1 // falling
		}
	}

	tc := state.CPR.TC
	bc := state.CPR.BC

	// 10. LONG signal
	if closePrice.GreaterThan(currentVwap) &&
		closePrice.GreaterThan(tc) && state.LastClose.LessThanOrEqual(tc) &&
		rsiVal.GreaterThan(decimal.NewFromFloat(s.cfg.RSILongThreshold)) &&
		vwapSlope >= 0 {

		stopLoss := closePrice.Sub(atrVal.Mul(decimal.NewFromFloat(s.cfg.SLMultiplier)))
		target := closePrice.Add(atrVal.Mul(decimal.NewFromFloat(s.cfg.TargetMultiplier)))

		state.SignaledToday = true
		return &models.Signal{
			Symbol:      candle.Symbol,
			Type:        models.BuySignal,
			ProductType: "MIS",
			Price:       closePrice,
			StopLoss:    stopLoss.Round(2),
			Target:      target.Round(2),
			Metadata: map[string]string{
				"Strategy":  "CPRVWAP_Long",
				"Pivot":     state.CPR.Pivot.StringFixed(2),
				"TC":        tc.StringFixed(2),
				"BC":        bc.StringFixed(2),
				"VWAP":      currentVwap.StringFixed(2),
				"EMA9":      ema9Val.StringFixed(2),
				"ATR":       atrVal.StringFixed(2),
				"ADX":       adxVal.StringFixed(2),
				"RSI":       rsiVal.StringFixed(2),
				"Volume":    volume.StringFixed(0),
				"AvgVol":    prevAvgVol.StringFixed(0),
				"CPRWidth%": decimal.NewFromFloat(widthPct).StringFixed(2),
				"CandleTime": candle.StartTime.In(s.loc).Format("2006-01-02 15:04:05"),
			},
		}
	}

	// 11. SHORT signal
	if closePrice.LessThan(currentVwap) &&
		closePrice.LessThan(bc) && state.LastClose.GreaterThanOrEqual(bc) &&
		rsiVal.LessThan(decimal.NewFromFloat(s.cfg.RSIShortThreshold)) &&
		vwapSlope <= 0 {

		stopLoss := closePrice.Add(atrVal.Mul(decimal.NewFromFloat(s.cfg.SLMultiplier)))
		target := closePrice.Sub(atrVal.Mul(decimal.NewFromFloat(s.cfg.TargetMultiplier)))

		state.SignaledToday = true
		return &models.Signal{
			Symbol:      candle.Symbol,
			Type:        models.SellSignal,
			ProductType: "MIS",
			Price:       closePrice,
			StopLoss:    stopLoss.Round(2),
			Target:      target.Round(2),
			Metadata: map[string]string{
				"Strategy":  "CPRVWAP_Short",
				"Pivot":     state.CPR.Pivot.StringFixed(2),
				"TC":        tc.StringFixed(2),
				"BC":        bc.StringFixed(2),
				"VWAP":      currentVwap.StringFixed(2),
				"EMA9":      ema9Val.StringFixed(2),
				"ATR":       atrVal.StringFixed(2),
				"ADX":       adxVal.StringFixed(2),
				"RSI":       rsiVal.StringFixed(2),
				"Volume":    volume.StringFixed(0),
				"AvgVol":    prevAvgVol.StringFixed(0),
				"CPRWidth%": decimal.NewFromFloat(widthPct).StringFixed(2),
				"CandleTime": candle.StartTime.In(s.loc).Format("2006-01-02 15:04:05"),
			},
		}
	}

	return nil
}
