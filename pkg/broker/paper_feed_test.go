package broker

import (
	"errors"
	"sync"
	"testing"
	"time"

	"zerobha/internal/models"
)

// fakeFeed records what the broker asked the tick feed to do.
type fakeFeed struct {
	mu           sync.Mutex
	subscribed   []string
	unsubscribed []string
	failWith     error
}

func (f *fakeFeed) Subscribe(symbol string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return f.failWith
	}
	f.subscribed = append(f.subscribed, symbol)
	return nil
}

func (f *fakeFeed) Unsubscribe(symbol string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return f.failWith
	}
	f.unsubscribed = append(f.unsubscribed, symbol)
	return nil
}

func (f *fakeFeed) calls() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.subscribed...), append([]string(nil), f.unsubscribed...)
}

// The contract a signal picks is not in the watchlist the trader subscribes at
// connect time, so the broker has to put it on the feed itself — otherwise
// nothing ticks for the instrument the position lives in, and its stop can only
// be evaluated at the quote monitor's polling interval.
func TestPaperSubscribesTheInstrumentItHoldsAPositionIn(t *testing.T) {
	feed := &fakeFeed{}
	p := NewPaperAdapter(nil, rs(1000000), WithPaperTickFeed(feed))

	o := entry("NIFTY26AUG24200CE", models.BuySignal, 650, 400)
	o.StopLoss = rs(380)
	mustPlace(t, p, o)

	subs, unsubs := feed.calls()
	if len(subs) != 1 || subs[0] != "NIFTY26AUG24200CE" {
		t.Fatalf("subscribed = %v, want the contract once", subs)
	}
	if len(unsubs) != 0 {
		t.Errorf("unsubscribed %v while the position is open", unsubs)
	}

	// Closing releases it: nothing needs ticks for a flat instrument.
	if _, err := p.ClosePosition("NIFTY26AUG24200CE", models.BuySignal, rs(420), time.Now(), "trail"); err != nil {
		t.Fatalf("close failed: %v", err)
	}
	_, unsubs = feed.calls()
	if len(unsubs) != 1 || unsubs[0] != "NIFTY26AUG24200CE" {
		t.Errorf("unsubscribed = %v, want the contract once after the close", unsubs)
	}
}

// A stop firing off a tick closes the position, which must also release the
// subscription — this is the path every Donchian option exit takes.
func TestPaperUnsubscribesWhenAStopFires(t *testing.T) {
	feed := &fakeFeed{}
	p := NewPaperAdapter(nil, rs(1000000), WithPaperTickFeed(feed))

	o := entry("NIFTY26AUG24200CE", models.BuySignal, 650, 400)
	o.StopLoss = rs(380)
	mustPlace(t, p, o)

	p.OnTick("NIFTY26AUG24200CE", rs(379), time.Now())

	if has, _ := p.HasOpenPosition("NIFTY26AUG24200CE"); has {
		t.Fatal("stop did not fire")
	}
	if _, unsubs := feed.calls(); len(unsubs) != 1 {
		t.Errorf("unsubscribed = %v, want one release after the stop", unsubs)
	}
}

// A feed that refuses must not take the position down with it: the quote
// monitor still covers the instrument, so the stop loses resolution rather than
// disappearing.
func TestPaperSurvivesAFeedFailure(t *testing.T) {
	feed := &fakeFeed{failWith: errors.New("websocket not connected")}
	p := NewPaperAdapter(nil, rs(1000000), WithPaperTickFeed(feed))

	o := entry("NIFTY26AUG24200CE", models.BuySignal, 650, 400)
	o.StopLoss = rs(380)
	filled, err := p.PlaceOrder(o)
	if err != nil {
		t.Fatalf("a failing tick feed must not fail the order: %v", err)
	}
	if filled.Status != models.OrderFilled {
		t.Errorf("order status = %s, want FILLED", filled.Status)
	}

	// The stop still works, because OnTick is fed by the monitor too.
	p.OnTick("NIFTY26AUG24200CE", rs(379), time.Now())
	if has, _ := p.HasOpenPosition("NIFTY26AUG24200CE"); has {
		t.Error("stop did not fire despite the position being marked")
	}
}

// After a restart the book is restored from the store, and those instruments
// have to go back on the feed — the trader's OnConnect only covers the
// watchlist.
func TestPaperResyncFeedSubscribesOpenPositions(t *testing.T) {
	feed := &fakeFeed{}
	p := NewPaperAdapter(nil, rs(1000000))

	mustPlace(t, p, entry("AAA", models.BuySignal, 10, 100))
	mustPlace(t, p, entry("BBB", models.BuySignal, 10, 100))
	if _, err := p.ClosePosition("BBB", models.BuySignal, rs(110), time.Now(), "done"); err != nil {
		t.Fatalf("close failed: %v", err)
	}

	// Feed attached only now, as the trader does once its socket is up.
	p.SetTickFeed(feed)
	p.ResyncFeed()

	subs, _ := feed.calls()
	if len(subs) != 1 || subs[0] != "AAA" {
		t.Errorf("subscribed = %v, want only the still-open position", subs)
	}
}

// Guards the lock discipline: the feed is a network call and must never be
// invoked while the broker's mutex is held, or a slow socket would block every
// balance and position read on the candle hot path.
func TestPaperFeedIsCalledWithoutHoldingTheLock(t *testing.T) {
	feed := &lockProbingFeed{}
	p := NewPaperAdapter(nil, rs(1000000), WithPaperTickFeed(feed))
	feed.p = p

	mustPlace(t, p, entry("AAA", models.BuySignal, 10, 100))

	if !feed.observed {
		t.Fatal("feed was never called")
	}
	if feed.blocked {
		t.Error("feed was called while the broker's lock was held")
	}
}

// lockProbingFeed tries to take the broker's lock from inside the callback.
type lockProbingFeed struct {
	p        *PaperAdapter
	observed bool
	blocked  bool
}

func (f *lockProbingFeed) Subscribe(symbol string) error {
	f.observed = true
	done := make(chan struct{})
	go func() {
		f.p.mu.Lock()
		f.p.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		f.blocked = true
	}
	return nil
}

func (f *lockProbingFeed) Unsubscribe(symbol string) error { return nil }

var _ TickSubscriber = (*fakeFeed)(nil)
