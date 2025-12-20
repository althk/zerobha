package strategy

import (
	"zerobha/internal/core"
	"zerobha/internal/models"

	"github.com/shopspring/decimal"
)

// AlwaysBuy strategy only issues buy signals all the time.
type AlwaysBuy struct{}

func (a *AlwaysBuy) Name() string {
	return "AlwaysBuy"
}

func (a *AlwaysBuy) Init(provider core.DataProvider) error {
	// nothing to do
	return nil
}

func (a *AlwaysBuy) OnCandle(candle models.Candle) *models.Signal {
	return &models.Signal{
		Symbol:   candle.Symbol,
		Type:     models.BuySignal,
		Price:    candle.Close,
		StopLoss: candle.Low,
		Target:   candle.High.Add(decimal.NewFromInt(10)),
	}
}
