package main

import (
	"reflect"
	"testing"

	"zerobha/internal/core"
	"zerobha/internal/models"
)

type stubStrategy string

func (s stubStrategy) Name() string                        { return string(s) }
func (stubStrategy) Init(core.DataProvider) error          { return nil }
func (stubStrategy) OnCandle(models.Candle) *models.Signal { return nil }

func lr(name string, squareOff int) *liveRunner {
	return &liveRunner{runner: &core.Runner{Strategy: stubStrategy(name)}, squareOffMin: squareOff}
}

// The account-wide square-off runs at the latest strategy's time, so a
// strategy holding until 15:15 is not flattened by another's 14:57; the
// earlier one closes only its own positions at its own time.
func TestSquareOffPlan(t *testing.T) {
	global, early := squareOffPlan([]*liveRunner{lr("Donchian", 14*60+57), lr("emacross", 15*60+15), lr("ORB", defaultSquareOffMin)})
	if global != 15*60+15 {
		t.Errorf("global square-off = %d, want 915", global)
	}
	want := map[int][]string{14*60 + 57: {"Donchian"}, defaultSquareOffMin: {"ORB"}}
	if !reflect.DeepEqual(early, want) {
		t.Errorf("early = %v, want %v", early, want)
	}

	// One strategy: its own time, nothing early - the old behaviour.
	global, early = squareOffPlan([]*liveRunner{lr("Donchian", 14*60+57)})
	if global != 14*60+57 || len(early) != 0 {
		t.Errorf("single strategy: global %d early %v, want 897 and none", global, early)
	}
}
