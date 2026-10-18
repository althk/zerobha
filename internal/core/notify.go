package core

import (
	"fmt"
	"strings"

	"zerobha/internal/models"
	"zerobha/pkg/notify"
)

// notify forwards to the configured notifier; nil sends nothing.
func (e *Engine) notify(level notify.Level, msg string) {
	notify.Or(e.Notifier).Notify(level, msg)
}

// modeTag prefixes paper notifications so they are never mistaken for real
// fills in the chat.
func (e *Engine) modeTag() string {
	if e.PaperMode {
		return "[PAPER] "
	}
	return ""
}

// entryMessage describes a placed entry order. An option leg also carries the
// index levels the strategy actually trades on.
func (e *Engine) entryMessage(strategy string, o models.Order) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s🟢 ENTRY %s: %s %s x%s @ %s", e.modeTag(), strategy, o.Side, o.Symbol,
		o.Quantity.String(), o.Price.StringFixed(2))
	if o.StopLoss.IsPositive() {
		fmt.Fprintf(&b, "\nStop %s", o.StopLoss.StringFixed(2))
	}
	if o.Target.IsPositive() {
		fmt.Fprintf(&b, "  Target %s", o.Target.StringFixed(2))
	}
	m := o.Metadata
	if u := m["Underlying"]; u != "" && u != o.Symbol {
		fmt.Fprintf(&b, "\n%s %s view: entry %s, index stop %s", u, m["IndexSide"], m["IndexEntry"], m["IndexStop"])
		if m["Expiry"] != "" {
			fmt.Fprintf(&b, "\nExpiry %s (%s DTE)", m["Expiry"], m["DaysToExpiry"])
		}
	}
	if r := m["Reason"]; r != "" {
		fmt.Fprintf(&b, "\nReason: %s", r)
	}
	if o.ID != "" {
		fmt.Fprintf(&b, "\nOrder %s", o.ID)
	}
	return b.String()
}
