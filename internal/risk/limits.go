package risk

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// SetMonthPnL tells the manager where the month stands: realised PnL of the
// month's earlier sessions plus today's PnL as the broker reports it. Like
// SetDayPnL it is fed by the engine before every signal, because no exit
// passes through the risk manager.
func (rm *Manager) SetMonthPnL(pnl decimal.Decimal) {
	rm.monthPnL = pnl
}

// StockRiskBudget returns how many rupees a new entry on underlying may put at
// stake under MaxLossPerStockPerDay, counted across every strategy:
//
//	limit - today's loss on the underlying - risk of its positions still open
//
// dayPnL is the underlying's PnL today (realised plus mark-to-market) and only
// a loss counts against the budget — a profitable morning does not buy a
// bigger afternoon. isOpen reports whether a traded symbol still holds a
// position; entries that have since closed stop counting as open risk, their
// outcome is in dayPnL instead. An open position's mark-to-market loss and its
// entry risk overlap, which errs on the side of a smaller budget.
//
// limited is false when the limit is off. An exhausted budget is an error so
// the engine records it as a risk block, like every other limit here.
func (rm *Manager) StockRiskBudget(underlying string, dayPnL decimal.Decimal, isOpen func(symbol string) bool) (budget decimal.Decimal, limited bool, err error) {
	if !rm.MaxLossPerStockPerDay.IsPositive() {
		return decimal.Zero, false, nil
	}
	lost := decimal.Max(dayPnL.Neg(), decimal.Zero)
	if lost.GreaterThanOrEqual(rm.MaxLossPerStockPerDay) {
		return decimal.Zero, true, fmt.Errorf("risk rejection: daily loss limit for %s hit (lost Rs %s, limit Rs %s)",
			underlying, lost.StringFixed(0), rm.MaxLossPerStockPerDay.StringFixed(0))
	}
	open := decimal.Zero
	for symbol, er := range rm.openRisk {
		if er.Underlying == underlying && isOpen(symbol) {
			open = open.Add(er.Risk)
		}
	}
	budget = rm.MaxLossPerStockPerDay.Sub(lost).Sub(open)
	if !budget.IsPositive() {
		return decimal.Zero, true, fmt.Errorf("risk rejection: risk budget for %s used up (lost Rs %s + open risk Rs %s, limit Rs %s)",
			underlying, lost.StringFixed(0), open.StringFixed(0), rm.MaxLossPerStockPerDay.StringFixed(0))
	}
	return budget, true, nil
}
